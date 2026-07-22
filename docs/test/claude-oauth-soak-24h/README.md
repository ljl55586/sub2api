# Claude OAuth 单账号 24 小时 curl 稳定性测试

本文档用于在只有一个 Claude OAuth 账号的 sub2api 环境中执行一次保守的 24 小时 soak test。测试的目标是验证请求转换、粘性会话、缓存、用量记录和长时间运行稳定性，不用于寻找 OAuth 账号的最大吞吐量。

全部上行业务请求都由 `curl` 发出。脚本只使用 Bash 和 `jq` 生成 JSON、维护多轮消息历史、控制等待时间和保存结果，不使用其他 HTTP 客户端。

一天计划发送 90 个成功主请求：

- 3 条独立会话；
- 每条会话 30 轮；
- 60 个预期 cache hit，约 66.7%；
- 27 个同会话 TTL miss，约 30.0%；
- 3 个新会话 cold miss，约 3.3%；
- 任意滚动 5 小时最多 18 个成功请求；
- 同一时刻最多三个请求在途，每条会话最多一个；
- 不显式发送 `stream`，因此不触发当前分支的 quota/title 伴生请求。

90 个请求只是这套提示词和固定调度能够提供的测试容量，不代表一定能把 Claude Pro 的每个 5h 窗口推进到 80%。上游 utilization 会受到历史长度、输出长度、模型和缓存状态影响；脚本会读取 sub2api 已保存的被动用量用于限额保护，但不会为了追目标自动重复旧问题或自行加压。

## 文件说明

```text
claude-oauth-soak-24h/
├── README.md
├── schedule.tsv
├── run_24h.sh
├── send_turn.sh
├── summarize.sh
├── usage_guard.sh
├── lib/
│   └── runtime.sh
├── sessions/
│   ├── session-a.sh
│   ├── session-b.sh
│   └── session-c.sh
└── prompts/
    ├── session-a-go-log-analyzer.md
    ├── session-b-booking-service.md
    ├── session-c-redis-delay-queue.md
    └── followups.json
```

三个项目包分别是：

- A：Go 网关日志聚合器，讨论乱序、重试、重复计费和跨日归档；
- B：TypeScript + PostgreSQL 会议室预约服务，讨论并发 hold、幂等和状态竞争；
- C：Go + Redis 延迟队列，讨论 lease、丢任务、重复执行和 fencing。

每个首轮提示词都由真实的软件开发材料组成，包括背景、约束、现有代码、测试和脱敏日志。后续 29 轮按“分析—收敛方案—最小实现—确定性测试—性能—生产故障—灰度—发布评审”的顺序推进，不使用重复废话凑长度。

三个 `sessions/*.sh` 可以分别运行，也可以由 `run_24h.sh` 根据 `schedule.tsv` 统一并行调度。每个 wave 同时启动 A、B、C 三个脚本；单条会话内部仍严格串行，只有上一轮成功写入历史后才会发送下一轮。

## 0. 服务器正式运行：完整流程

下面是一条从发布测试包到完成汇总的完整路径。第一次运行建议保留阶段检查点，不要设置 `SOAK_AUTO_CONTINUE=1`。

### 0.1 在本机提交并推送测试包

```bash
cd /Users/ling/sub2api
git status --short --branch
git add .gitignore docs/test/claude-oauth-soak-24h
git commit -m "test: add parallel Claude OAuth soak runner"
git push myfork codex/claude-request-alignment
```

只提交上述测试包和 `.gitignore`，不要把本地抓包、API key、run 输出或其他无关目录加入提交。

### 0.2 在服务器更新脚本

```bash
cd /root/sub2api-src
git fetch origin
git checkout codex/claude-request-alignment
git pull --ff-only origin codex/claude-request-alignment

cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
chmod 700 run_24h.sh send_turn.sh summarize.sh usage_guard.sh \
  init_session_identities.sh verify_session_identities.sh sessions/*.sh
```

如果本次提交只修改 `docs/test/claude-oauth-soak-24h` 和 `.gitignore`，不需要重新构建 sub2api 镜像。如果同时更新了后端转发代码，仍需按正常部署流程重新 build 并 recreate `sub2api` 容器。

### 0.3 确认账号页面配置

正式运行前，在 sub2api 管理页面按第 2 节设置：

- 账号并发 `3`；
- 5h 窗口费用控制开启，暂用 `$11.20 + $2.60`；
- 会话数量控制开启，最大会话数 `3`；
- RPM 限制开启，基础 RPM `6`、分层限流、粘性缓冲 `3`；
- 用户消息限速选择“软性限速”，不能选择“串行队列”；
- `session_id_masking_enabled`、metadata passthrough、messages cache rewrite 保持关闭。

### 0.4 检查服务器、依赖和 Claude 账号 ID

```bash
cd /root/sub2api-deploy
docker compose ps
curl -fsS http://127.0.0.1:8080/health

command -v curl
command -v jq
command -v flock
command -v tmux
command -v od
```

并行模式必须有 `flock`。在 Debian/Ubuntu 上，如果缺失，它通常由 `util-linux` 软件包提供；安装完成后再继续。

查询 Claude 账号的数据库 ID，不要仅凭管理页面顺序猜测：

```bash
cd /root/sub2api-deploy
docker compose exec -T postgres sh -c \
  'psql -X -A -F "|" -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c \
  "SELECT id,name,platform,type,status,schedulable FROM accounts WHERE deleted_at IS NULL ORDER BY id;"'
```

记录目标 Claude OAuth 账号第一列的 `id`。以下示例假设它是 `1`。

### 0.5 创建观察目录并记录 debug 日志起点

```bash
mkdir -p /root/sub2api-soak-observation
stat -c '%s' /root/sub2api-deploy/data/gateway_debug.log \
  > /root/sub2api-soak-observation/gateway-debug-start-offset.txt
```

不要删除或重命名正在被 sub2api 写入的 `gateway_debug.log`。

### 0.6 进入 tmux，并在 tmux 内设置环境

```bash
tmux new -s sub2api-soak
```

进入 tmux 后执行。API key 通过隐藏输入读取，不会直接出现在 shell 历史中：

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h

export SOAK_BASE_URL='http://127.0.0.1:8080'
read -rsp 'sub2api test API key: ' SOAK_API_KEY
export SOAK_API_KEY
printf '\n'

export SOAK_MODEL='claude-opus-4-8'
export SOAK_MAX_TOKENS='1536'
export SOAK_USER_AGENT='soak-curl/1.0'
export SOAK_PARALLEL_SESSIONS='1'

export SOAK_ACCOUNT_ID='1'
export SOAK_DEPLOY_DIR='/root/sub2api-deploy'
export SOAK_IDENTITY_HOME='/root/sub2api-soak-identity'
export SOAK_USAGE_GUARD_MODE='required'
export SOAK_USAGE_5H_STOP_PERCENT='78'
export SOAK_USAGE_7D_STOP_PERCENT='70'

# 确保没有遗留的测试/跳过开关
unset SOAK_VALIDATE_ONLY SOAK_DRY_RUN SOAK_SKIP_WAITS SOAK_TEST_MODE SOAK_AUTO_CONTINUE
```

### 0.7 验证数据库用量保护

```bash
./usage_guard.sh
```

正常输出格式为：

```text
5h百分比|7d百分比|5h重置Unix时间|上游采样时间|样本年龄秒数
```

如果 5h/7d 两项都有数字，继续即可。如果这是该账号第一次经此版本转发而尚无被动样本，字段可能为空；正式运行时公共锁只允许 A、B、C 中的一路发出 bootstrap 请求，其余两路等待。bootstrap 成功后仍拿不到 5h/7d 数据，脚本会 fail-closed。

### 0.8 用独立目录执行无网络计划校验

```bash
export SOAK_OUTPUT_DIR="/root/sub2api-soak-validation/validate-$(date -u +%Y%m%dT%H%M%SZ)"
export SOAK_VALIDATE_ONLY=1
./run_24h.sh
unset SOAK_VALIDATE_ONLY
```

应看到类似：

```text
schedule validated: 30 burst rows grouped into parallel waves
session identities ready: file=.../session-identities.json device=............ sessions=3
SOAK_VALIDATE_ONLY=1; schedule validation completed without sending requests
```

这个步骤不会调用 sub2api 或 Anthropic。校验目录已经非空，不能用于正式运行。

验证 A、B、C 共用一个 device ID、各自拥有不同的固定 session ID：

```bash
./verify_session_identities.sh "$SOAK_OUTPUT_DIR"
```

`SOAK_IDENTITY_HOME` 中的 `account-1.device-id` 是账号级持久身份，后续正式运行和重新测试都会复用；每个新 `SOAK_OUTPUT_DIR` 则生成一组新的 A/B/C session ID。

### 0.9 创建全新的正式输出目录并启动

```bash
export SOAK_OUTPUT_DIR="/root/sub2api-soak-runs/run-$(date -u +%Y%m%dT%H%M%SZ)"
printf 'formal output: %s\n' "$SOAK_OUTPUT_DIR"
./run_24h.sh
```

启动后先进入 5–15 分钟的随机等待是正常现象。第一组 wave 随后会并行启动 A1、B1、C1；如果需要 bootstrap 用量样本，则先只放行其中一路。

正式启动后可再次执行身份校验；已有请求时，它还会逐个检查请求体内的 metadata 是否与该 session 的固定身份一致：

```bash
./verify_session_identities.sh "$SOAK_OUTPUT_DIR"
```

不要在同一个 `SOAK_OUTPUT_DIR` 上再次启动第二个 `run_24h.sh`。脚本发现目录非空会拒绝启动。

### 0.10 SSH 断线、监控和阶段放行

从 tmux 脱离但不停止压测：按 `Ctrl-b`，松开后按 `d`。重新连接：

```bash
tmux attach -t sub2api-soak
```

另开 SSH 终端监控：

```bash
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* | head -n 1)
tail -F "$run_dir/run.log"
```

另一个终端查看 sub2api 和上游请求快照：

```bash
cd /root/sub2api-deploy
docker compose logs --tail=100 -f sub2api
```

```bash
tail -F /root/sub2api-deploy/data/gateway_debug.log
```

每个阶段完成并到达下一个 5h 时间点后，脚本会打印一个完整的 `touch .../continue-phase-N` 命令。先检查管理页面中的 5h、7d、费用、错误和账号状态，确认正常后原样执行该命令。第一次正式测试不要启用自动放行。

### 0.11 手动停止

推荐优雅停止，不再创建新请求，但允许最多三个在途请求完成：

```bash
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* | head -n 1)
touch "$run_dir/control/STOP"
```

需要立即中断在途 curl：

```bash
tmux send-keys -t sub2api-soak C-c
touch "$run_dir/control/STOP"
```

### 0.12 结束后汇总

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* | head -n 1)
./summarize.sh "$run_dir"

column -t -s $'\t' "$run_dir/manifest.tsv" | less -S
find "$run_dir/control" -maxdepth 1 -type f -print
du -sh "$run_dir" /root/sub2api-deploy/data/gateway_debug.log
```

最后记录 Claude 管理页面的 5h/7d 用量、reset 时间和 sub2api 当前窗口费用，再按第 12 节判断本轮是否通过。

## 1. 测试前提

推荐直接在 sub2api 所在服务器上运行脚本，并访问：

```text
http://127.0.0.1:8080/v1/messages
```

这样下游 API key 不经过公网明文传输。sub2api 转发给 Anthropic 的连接仍然是 HTTPS。

开始前确认：

1. 服务器时间和时区正常，NTP 已同步。
2. `docker compose ps` 中 sub2api、PostgreSQL、Redis 都是健康状态。
3. Claude OAuth 账号没有处于限流或不可调度状态。
4. 官方 Claude Code `/model` 中该账号确实能使用计划测试的模型。
5. Claude Usage 页面当前 5h 用量较低，周用量最好低于 30%。
6. 使用一把专门的 sub2api 测试 API key；它所属分组只包含这个 Claude 账号。
7. 账号的 `session_id_masking_enabled` 保持关闭。
8. metadata passthrough 保持关闭。
9. messages cache rewrite 保持关闭，由测试脚本管理 breakpoint。
10. 账号 cache TTL override 关闭，或目标明确设为 `5m`。

如果测试目的是测 sub2api 的最大并发或最大吞吐，不应使用本方案和个人 OAuth 订阅，应改用 Claude Console API/PAYG 测试账号。

## 2. sub2api 账号配置

在账号编辑页面设置：

| 项目 | 值 |
|---|---:|
| 账号并发 | 3 |
| 5h 窗口费用控制 | 开启 |
| 费用阈值 | `$11.20`（按实测样本暂定） |
| 粘性预留额度 | `$2.60`（按实测样本暂定） |
| 会话数量控制 | 开启 |
| 最大会话数 | 3 |
| 会话空闲超时 | 60 分钟 |
| RPM 限制 | 开启 |
| 基础 RPM | 6 |
| RPM 策略 | 分层限流 |
| RPM 粘性缓冲 | 3 |
| 用户消息限速 | 软性限速 |

不要把粘性预留保留成 UI 默认的 `$10`。按 2026-07-22 的两请求样本，本地标准费用 `$0.171667` 约对应 5h utilization 增加 1 个百分点，因此暂以约 65% 对应的 `$11.20` 作为普通阈值、约 15% 对应的 `$2.60` 作为粘性预留，最终硬线约为 `$13.80`。管理页面的百分比可能经过取整，正式运行前仍应使用数据库原始 utilization 差值校准。

费用限制是基于 usage log 的标准 API 折算金额，不是 Claude Pro/Max 用量百分比，而且是软限制。数据库或 Redis 查询异常时相关代码可能 fail-open，所以脚本自己的滚动请求计数和人工 5h/7d 检查仍然必须保留。

“串行队列”会按账号 ID 获取全局锁，会把 A、B、C 三路重新排成串行，因此本并行测试不能选择它。“软性限速”只增加 RPM 感知的短延迟，不获取账号级串行锁。账号并发小于 3 时，第三路会在 sub2api 等待账号槽位；基础 RPM 太小则并行 wave 可能被 RPM 调度挡住。

## 3. 在服务器准备测试包

先把当前分支提交并推送，然后在服务器更新源码。测试包只是文档和脚本，不需要重新构建 sub2api 镜像：

```bash
cd /root/sub2api-src
git fetch origin
git checkout codex/claude-request-alignment
git pull --ff-only origin codex/claude-request-alignment

cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
chmod 700 run_24h.sh send_turn.sh summarize.sh usage_guard.sh sessions/*.sh
```

检查依赖：

```bash
curl --version
jq --version
flock --version
cd /root/sub2api-deploy
docker compose ps
curl -fsS http://127.0.0.1:8080/health
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
```

如果服务器没有 `jq`，先用系统包管理器安装。不要为了省略 `jq` 把完整历史手工拼成 JSON，手工转义代码和模型响应很容易破坏会话前缀。

记录 debug 日志起始位置，不需要删除或改名原文件：

```bash
mkdir -p /root/sub2api-soak-observation
stat -c '%s' /root/sub2api-deploy/data/gateway_debug.log \
  > /root/sub2api-soak-observation/gateway-debug-start-offset.txt
```

## 4. 设置运行环境

不要把 API key 写进仓库、提示词、命令历史文件或 `.env`。在即将运行测试的 shell/tmux 中导出：

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h

export SOAK_BASE_URL='http://127.0.0.1:8080'
read -rsp 'sub2api test API key: ' SOAK_API_KEY
export SOAK_API_KEY
printf '\n'
export SOAK_MODEL='claude-opus-4-8'
export SOAK_MAX_TOKENS='1536'
export SOAK_USER_AGENT='soak-curl/1.0'
export SOAK_OUTPUT_DIR="/root/sub2api-soak-runs/run-$(date -u +%Y%m%dT%H%M%SZ)"
export SOAK_PARALLEL_SESSIONS='1'

# 自动用量保护直接读取本机 sub2api PostgreSQL
export SOAK_ACCOUNT_ID='1'
export SOAK_DEPLOY_DIR='/root/sub2api-deploy'
export SOAK_IDENTITY_HOME='/root/sub2api-soak-identity'
export SOAK_USAGE_GUARD_MODE='required'
export SOAK_USAGE_5H_STOP_PERCENT='78'
export SOAK_USAGE_7D_STOP_PERCENT='70'
```

同一次测试中不要更换 API key、`SOAK_USER_AGENT`、`SOAK_ACCOUNT_ID`、`SOAK_IDENTITY_HOME` 或运行主机。初始化器为同一账号保存一个 64 位十六进制 device ID，并为每个新 run 的 A、B、C 分别生成一个 UUID。每次请求都携带该会话固定的 `metadata.user_id.session_id`，所以 sub2api 在选账号和检查 `max_sessions` 之前就能稳定识别三条会话。

`session-a`、`session-b`、`session-c` 是本地映射名；发给 sub2api 的是对应 UUID，不是这些文字名称。metadata passthrough 必须保持关闭，因此下游 metadata 不会原样发给 Anthropic；OAuth 模拟逻辑仍会按账号指纹和第一条 user 内容生成稳定的上游 session ID。

`SOAK_USAGE_GUARD_MODE=required` 表示读取用量失败时 fail-closed，不再发送请求。首次运行尚无被动用量样本时，脚本最多放行一个 bootstrap 请求；该响应应该让 sub2api 保存 5h/7d 响应头，下一次请求前如果仍读不到数据就会停止。`best_effort` 会在读取失败时继续，不适合保护 Claude Pro 账号；`off` 只应用于不联网的脚本测试。

启动压测前可以先检查数据库快照：

```bash
./usage_guard.sh
```

输出依次是 `5h百分比|7d百分比|5h重置Unix时间|上游采样时间|样本年龄秒数`。该脚本只读取本机 PostgreSQL，不会向 Anthropic 发送 quota 请求。

## 5. 先做不联网的请求体检查

`SOAK_DRY_RUN=1` 只生成第一轮 JSON，不调用 curl：

```bash
export SOAK_DRY_RUN=1
./send_turn.sh \
  session-a \
  prompts/session-a-go-log-analyzer.md \
  cold
unset SOAK_DRY_RUN
```

查看生成的请求：

```bash
jq '{
  model,
  max_tokens,
  metadata:(.metadata.user_id | fromjson),
  message_count:(.messages|length),
  last_message:.messages[-1]
}' \
  "$SOAK_OUTPUT_DIR"/requests/*.json

./verify_session_identities.sh "$SOAK_OUTPUT_DIR"
```

必须确认：

- `model` 正确；
- `max_tokens` 是 1536；
- 没有 `stream` 字段；
- 没有 `tools`；
- metadata 中的 device ID 是 64 位十六进制，session ID 与 `session-identities.json` 一致；
- 只有最新 user content 带 `cache_control`；
- TTL 是 `5m`，没有 `1h`。

正式运行时必须换一个全新的 `SOAK_OUTPUT_DIR`，不要复用 dry-run 目录。

三个会话脚本也可以单独调用。下面会发送 Session A 的第 1–3 轮；它是实际请求，不是 dry-run：

```bash
./sessions/session-a.sh 1 cold 300 900
```

四个参数分别是 burst 首轮编号、首轮缓存预期、首轮前最短等待秒数和最长等待秒数。后两轮固定为预期 hit，并分别随机等待 90–180 秒和 90–240 秒。多次单独调用必须复用同一个 `SOAK_OUTPUT_DIR`，否则历史状态和会话前缀会丢失。

实际发送前，公共 runtime 会核对参数中的计划轮次与 `state/<session>.messages.json` 推导出的下一轮是否一致；如果跳轮、重复运行已经完成的 burst 或拿错输出目录，会写入 `STOPPED_ON_STATE_MISMATCH` 并停止，不会把错误问题接到错误历史上。

## 6. 启动 24 小时测试

建议用 tmux，避免 SSH 断线结束进程：

```bash
tmux new -s sub2api-soak
```

进入 tmux 后重新执行第 4 节的 `export`，然后运行：

```bash
./run_24h.sh
```

脚本默认是“人工检查后继续”模式。每个阶段结束并到达下一阶段时间点后，它会等待一个控制文件。例如开始第二阶段前会显示：

```text
touch /root/sub2api-soak-runs/run-.../control/continue-phase-2
```

确认账号用量和服务状态正常后，在另一个终端执行输出的命令。后续依次是：

```bash
touch "$SOAK_OUTPUT_DIR/control/continue-phase-2"
touch "$SOAK_OUTPUT_DIR/control/continue-phase-3"
touch "$SOAK_OUTPUT_DIR/control/continue-phase-4"
touch "$SOAK_OUTPUT_DIR/control/continue-phase-5"
```

另一个终端需要先把 `SOAK_OUTPUT_DIR` 设成运行终端使用的同一路径；也可以直接复制脚本日志里打印出的完整 `touch /root/...` 命令。

第一次测试不建议无人值守。如果已经完成过一轮校准并确认额度足够，可以显式设置：

```bash
export SOAK_AUTO_CONTINUE=1
```

需要安全停止时执行：

```bash
touch "$SOAK_OUTPUT_DIR/control/STOP"
```

这是推荐的“优雅停止”：三个会话脚本在下一次最长 30 秒的检查周期内停止，不会再发送新请求；已经在途的最多三个 curl 会等待响应完成或在 900 秒时超时。

如果另一个 SSH 终端没有保留 `SOAK_OUTPUT_DIR`，先找到本次运行目录再停止：

```bash
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* | head -n 1)
touch "$run_dir/control/STOP"
```

需要立即中断在途 curl 时，在运行窗口按 `Ctrl-C`。如果运行在 tmux 中，也可以从另一个终端发送中断；随后仍建议写入 `STOP`，防止残留的并行子进程进入下一轮：

```bash
tmux send-keys -t sub2api-soak C-c
touch "$run_dir/control/STOP"
```

## 7. 一天的具体请求计划

| 阶段 | 相对时间 | 请求序列 | 数量 |
|---|---|---|---:|
| 1 | 0–5h | A1–A6、B1–B6、C1–C6 | 18 |
| 2 | 5–10h | A7–A12、B7–B12、C7–C12 | 18 |
| 3 | 10–15h | A13–A18、B13–B18、C13–C18 | 18 |
| 4 | 15–20h | A19–A24、B19–B24、C19–C24 | 18 |
| 5 | 20–24h | A25–A30、B25–B30、C25–C30 | 18 |

每三个连续请求组成一个同会话 burst：第一个请求在该会话静默超过 5 分钟后发送，预期为 cold/TTL miss；后两个问题紧接着追问，预期命中刚形成的长前缀缓存。每个阶段分成两个 wave，每个 wave 同时启动 A、B、C 三个 burst，因此最多有三条请求并行在途，但同一会话不会并发修改历史。

具体调度不再硬编码在总脚本中，而是保存在 `schedule.tsv`。总调度器读取阶段、wave、会话脚本、首轮编号、缓存预期和 wave 等待范围；同一 phase/wave 下的会话脚本会并行启动。

等待时间不是固定值：

- 每个 wave 启动前统一随机等待：第一组 5–15 分钟，第二组 8–25 分钟；
- A、B、C 启动后各自再随机错开 0–20 秒；
- 预期 hit 前随机等待 90–240 秒；
- 阶段剩余时间保持静默，直到下一个 5h 时间点。

脚本还维护共享的滚动 5h 成功时间戳和在途名额预留。如果“最近 5 小时成功请求 + 当前在途请求”已经达到 18，新的并行请求不会一起穿过检查，而会等待名额；共享文件通过 `flock` 加锁。

## 8. cache_control 和历史维护

每个 run 启动时先生成：

```text
session-identities.json
```

其中 A、B、C 共用账号级 device ID，但各自使用不同的 session UUID。`send_turn.sh` 每次发送前都会读取并校验该文件，把固定身份写入 `metadata.user_id`。身份文件不随 messages 增长而改变；缺失、格式错误、账号/device 不匹配或 session UUID 重复都会在发送前停止。

`send_turn.sh` 为每个 session 保存一个：

```text
state/session-a.messages.json
```

状态文件包含完整历史，但不保留旧的 `cache_control`。发送下一轮时，脚本：

1. 读取上次历史；
2. 追加当前 user；
3. 只在当前 user 的 text block 上添加 `cache_control: {type: ephemeral, ttl: 5m}`；
4. 用 curl 发送完整 messages；
5. 仅在 HTTP 2xx 且响应有 content 数组时，把当前 user 和上游返回的原始 assistant `content` 写入状态。

因此 thinking block、signature 或其他 assistant content 不会被拼成纯文本或丢失。任何失败请求都不会推进本地会话状态。

预期 hit 的请求在同一会话上一响应完成后 5 分钟内发送。缓存条目只有在前一个响应开始生成后才可读，因此每条会话内部严格串行，不会并发发送同一会话的预热和命中请求；不同会话之间允许并行。

如果自动 5h 暂停或滚动请求上限导致等待超过 5 分钟，runtime 会根据该会话上一响应的完成时间，把原计划的 `hit` 自动改记为 `ttl_miss`，避免统计结果仍错误标成缓存命中。

预期 TTL miss 的请求仍使用同一个 session 和完整历史，只是等待超过 5 分钟。新会话 cold miss 则使用另一份不同的首轮项目包，而且阶段之间已有超过 5 分钟的全局静默。

即使预期 cold/TTL miss，`cache_read_input_tokens` 也不一定为零，因为 sub2api 注入的公共 system prompt 可能仍命中。判断项目 messages 是否命中，要看 cache read 是否达到项目长前缀的量级。

## 9. 运行中监控

### 9.1 sub2api 日志

```bash
cd /root/sub2api-deploy
docker compose logs --tail=100 -f sub2api
```

另一个终端观察请求快照：

```bash
tail -F /root/sub2api-deploy/data/gateway_debug.log
```

本方案不带 `stream`，正常情况下不应出现：

```text
UPSTREAM_SESSION_COMPANION_QUOTA
UPSTREAM_SESSION_COMPANION_TITLE
```

每个主请求都应看到一组 `CLIENT_ORIGINAL` 和 `UPSTREAM_FORWARD`。同一项目会话中的 `x-claude-code-session-id` 应保持不变；A、B、C 三个项目之间应不同。

第一组 wave 发送到第 2 轮后，额外检查 sub2api 的调度 hash 来源：

```bash
cd /root/sub2api-deploy
docker compose logs --since 20m sub2api 2>&1 |
  grep -E 'sticky.hash_source|sticky.session_hash_generated'
```

`sticky.hash_source` 应显示 `source=metadata_user_id`。A1/A2/A3 应重复同一个 session UUID，B、C 同理，三组之间不同；不应再在 A2 出现第 4 个新调度会话。如果这里仍显示 `source=cacheable_content`，立即停止测试，说明服务器没有运行修正后的脚本请求体。

### 9.2 容器资源

可以在单独终端每分钟采样一次：

```bash
while true; do
  date -u +%Y-%m-%dT%H:%M:%SZ
  docker stats --no-stream --format '{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}'
  sleep 60
done | tee /root/sub2api-soak-observation/docker-stats.log
```

### 9.3 每个 5h 检查点

在管理员账号页面记录：

- 当前 5h utilization 和 reset 时间；
- 当前 7d utilization 和 reset 时间；
- `current_window_cost`；
- 账号是否仍为可调度；
- 当前 RPM；
- 活跃会话数；
- 最近错误。

第一阶段开始后先发送少量请求并记录数据库原始 utilization。用下面公式校准本地费用限制：

```text
K = 本地窗口费用增量 / 上游 5h utilization 增量
建议费用阈值 = K × 0.65
建议粘性预留 = K × 0.15
最终硬线 = K × 0.80
```

如果数据库中的原始利用率增量不是准确的 `0.01`，就不要照搬 `$11.20/$2.60`；使用实际差值重算。费用只是 sub2api 的本地保护代理，不能替代 Claude Usage 页面和上游 utilization 的停止判断。

## 10. 自动停止与人工停止条件

`send_turn.sh` 对所有非 2xx、curl 网络错误和异常响应都返回失败；`run_24h.sh` 随即停止整个测试，不自动重试。

在 `required` 模式下，每个请求随机等待前、真正发送前以及成功响应后都会运行一次自动保护：

- 从 PostgreSQL 的 `accounts.extra` 读取 `session_window_utilization` 和 `passive_usage_7d_utilization`；
- 5h utilization 达到 `SOAK_USAGE_5H_STOP_PERCENT` 时停止发送，等待 `session_window_end` 加 60–180 秒随机缓冲，窗口重置后重新检查再继续；
- 7d utilization 达到 `SOAK_USAGE_7D_STOP_PERCENT` 时创建 `STOP`，结束整个 24 小时任务，不会等周窗口重置；
- 查询失败、账号不存在、bootstrap 后仍缺少 5h/7d 样本，都会 fail-closed；
- 任意非 2xx、24h 截止或人工 `STOP` 也会停止后续请求；
- 滚动 5h 请求数量达到 18 时只暂停，等最早请求退出滚动窗口后继续。

默认把 5h 自动暂停线设为 78% 而不是 80%，是为了给最后一个已经发送但尚未反映在 utilization 中的请求留少量余量。如确认单次请求增量足够小，可以显式改成 `80`，但自动保护只能在请求之间判断，无法撤回已经发出的请求。

出现以下任一情况，不要创建下一阶段 continue 文件，直接创建 `STOP`：

- 任意 400、401 或 403；
- 任意 429；
- 账号进入限流、不可调度或异常状态；
- p95 延迟持续明显高于首阶段基线；
- Redis、PostgreSQL、sub2api health 异常；
- 服务器磁盘使用率达到 80%；
- `gateway_debug.log` 中同一计划请求出现意外的多次主转发。

即使启用了自动保护，7d utilization 达到 65% 后仍建议人工观察管理页面；数据库值来自最近一次上游响应头，不是独立实时探针。如果同一个 Claude 账号还在别处使用，数据库样本可能在两次 sub2api 请求之间落后于真实用量。24 小时中出现长静默不是测试失败，账号保护优先于完成请求数量。

## 11. 输出文件

每个 run 目录包含：

```text
run.meta
run.log
manifest.tsv
session-identities.json
successful-request-times.txt
inflight-request-reservations.tsv
usage-samples.tsv
requests/*.json
responses/*.json
headers/*.headers
state/*.messages.json
control/*
```

自动保护会在 `control/` 中留下原因文件，例如 `PAUSED_ON_5H_LIMIT`、`STOPPED_ON_7D_LIMIT`、`STOPPED_ON_USAGE_GUARD_ERROR`、`STOPPED_ON_STATE_MISMATCH` 或 `STOPPED_ON_ERROR`。

`manifest.tsv` 每行记录：

- session、turn 和预期缓存类别；
- HTTP 状态码和耗时；
- input/output tokens；
- cache creation/read tokens；
- 上游 message/request id。

测试结束或中途停止后运行：

```bash
./summarize.sh "$SOAK_OUTPUT_DIR"
```

默认用 4096 tokens 作为项目长前缀是否明显命中/写入的初筛阈值。它只是诊断阈值，不是 Anthropic API 的协议要求。查看被标记的请求：

```bash
column -t -s $'\t' "$SOAK_OUTPUT_DIR/manifest.tsv" | less -S
```

进一步比较某个请求：

```bash
jq '.usage' "$SOAK_OUTPUT_DIR"/responses/请求文件名.json
jq '.messages | length' "$SOAK_OUTPUT_DIR"/requests/请求文件名.json
```

## 12. 通过标准

本轮 24 小时测试可视为通过，需要同时满足：

- 没有 400、401、403、429；
- 每个 wave 最多三条不同会话请求并行，同一会话没有并发或重复发送；
- 所有下游请求共用账号级 device ID，A/B/C 各自的 metadata session ID 跨 30 轮稳定且互不相同；
- A、B、C 各自的上游 session ID 在多轮中稳定，三者之间不同；
- 预期 hit 样本大多数有长前缀量级的 `cache_read_input_tokens`；
- 预期 TTL/cold miss 大多数有相应的 `cache_creation_input_tokens`；
- sub2api、Redis、PostgreSQL 连续健康；
- 5h 和 7d 用量未越过停止阈值；
- 费用阈值、会话数、RPM 和软性限速没有出现绕过或异常放行；
- 日志和状态文件能够解释每一个请求的计划、实际上游请求和响应。

不要把项目回答质量作为本轮主要通过标准。项目包的作用是提供自然、稳定、可持续追加的长上下文；本轮重点仍是请求链路、会话和缓存行为。

## 13. 后续增加新会话

增加 Session D 时只需要：

1. 在 `prompts/` 新增首轮项目包；
2. 在 `followups.json` 增加 `session-d` 的后续问题数组；
3. 复制一个 `sessions/session-*.sh` 为 `sessions/session-d.sh`，只修改 session 名和首轮提示词路径；
4. 在 `schedule.tsv` 增加 Session D 的 burst；
5. 把 sub2api 的最大会话数从 3 调整到实际会话总数。

总调度器会从 `schedule.tsv` 自动发现 Session D，并在新 run 的 `session-identities.json` 中为它生成一个独立 UUID，不需要手工维护身份表。公共 curl、历史维护、随机等待、缓存分类、滚动请求上限和用量保护都在 `lib/runtime.sh`。同一个 phase/wave 中的会话会并行，所以新增会话后必须同时评估账号并发、RPM 和最大会话数；同一会话不能在同一个 wave 出现两次。
