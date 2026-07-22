# Claude OAuth 单账号五会话 curl 压测

本测试包只通过 `curl` 调用 sub2api `/v1/messages`，用于观察单个 Claude OAuth/Pro 账号的连续多轮、粘性会话、5 分钟提示缓存、并行调度和用量保护。

当前计划包含五个彼此独立、但各自连续推进的自然项目会话：

- Session A：Go 网关日志分析器，从乱序、重试、重复计费逐步讨论到 checkpoint、reconciliation、故障演练和发布；
- Session B：TypeScript + PostgreSQL 会议室预约服务，从并发 hold 和幂等逐步讨论到迁移、outbox、性能、主库切换和全量发布；
- Session C：Go + Redis 延迟任务队列，从 lease、重复执行和丢任务逐步讨论到 fencing、分桶、故障演练和发布；
- Session D：Java + Kafka + PostgreSQL 支付对账服务，从重复入账和提交结果未知逐步讨论到退款、账本不变量、历史修复和发布；
- Session E：Python + Celery + PostgreSQL 超大 CSV 导入，从断点恢复和重复批次逐步讨论到 replace 可见性、取消、容量和发布。

每个会话 30 轮，共 150 个主请求。五个会话在同一 wave 中并行，同一会话内部严格串行。

调度器不再划分固定的 5 个 phase，也没有“每个 phase 18 个请求”或“滚动 5h 最多 18 个请求”的默认限制。它连续执行 10 个 wave，真正控制发送的是上游用量保护：

- 5h utilization 达到 `78%`：停止发送，等待当前窗口重置后自动继续剩余请求；
- 7d utilization 达到 `70%`：终止整个运行；
- 150 个计划请求全部完成但 5h 仍低于阈值：正常结束，不重复旧问题，也不自动增加请求。

## 1. 文件和会话设计

```text
claude-oauth-soak-24h/
├── README.md
├── run_24h.sh
├── send_turn.sh
├── init_session_identities.sh
├── verify_session_identities.sh
├── usage_guard.sh
├── summarize.sh
├── schedule.tsv
├── lib/runtime.sh
├── sessions/
│   ├── session-a.sh
│   ├── session-b.sh
│   ├── session-c.sh
│   ├── session-d.sh
│   └── session-e.sh
└── prompts/
    ├── session-a-go-log-analyzer.md
    ├── session-b-booking-service.md
    ├── session-c-redis-delay-queue.md
    ├── session-d-payment-reconciliation.md
    ├── session-e-data-import-pipeline.md
    └── followups.json
```

### Session A：Go 网关日志分析器

首轮提供一份接近真实 code review 的长项目包，包含业务约束、事件协议、现有 Go 代码、测试和脱敏日志。后续 29 轮按照真实维护节奏推进：

1. 乱序、重试、重复 usage 和 finalize 不变量；
2. watermark、跨日归属、空闲日志源和大行读取；
3. 有界错误收集、时间戳异常、复合 request key；
4. pricing 版本、attempt 摘要、时钟偏差和确定性输出；
5. profile、gzip、checkpoint 和 usage reconciliation；
6. fuzz/property 测试、灰度事故、schema 演进和发布检查。

### Session B：PostgreSQL 预约服务

首轮包含业务状态、PostgreSQL 表结构、TypeScript 事务代码、竞态测试和生产日志。后续 29 轮持续沿同一项目推进：

1. exclusion constraint、advisory lock、条件更新和幂等；
2. 1800 万行脏数据迁移和确定性并发测试；
3. extend、expire、cancel、outbox 和 worker 竞争；
4. 热点查询、连接池、时钟、时区和权限边界；
5. expand/migrate/contract、invalid index 和冲突指标；
6. 主库故障切换、数据库不变量和最终 readiness review。

### Session C：Redis 延迟任务队列

首轮提供 Redis Cluster key 设计、Lua、Go worker、测试和故障日志。后续 29 轮覆盖 lease lost、fencing、原子 Retry/Dead、故障注入、业务资源串行、分桶、大 payload、resharding、容量和最终发布。

### Session D：支付对账与账本

首轮提供支付状态、账本和余额表、Kafka consumer、主动查询路径、部分退款、对账 SQL、测试与事故日志。后续 29 轮沿统一 capture 命令、提交结果未知、Kafka ack、退款并发、账本不变量、账单修订、历史冲正、迁移和发布逐步推进。

### Session E：超大 CSV 数据导入

首轮提供上传与 import 状态、Celery 配置、validate/apply/cancel 代码、staging 表、测试和故障日志。后续 29 轮沿 outbox、可恢复解析、重复 SKU、batch claim、delta 幂等、replace 原子可见性、取消、租户隔离、容量和发布逐步推进。

五份首轮项目包均为约 15–18KB 的真实项目材料，明显超过 Claude 缓存最低前缀量级。后续问题使用自然口吻引用先前结论，不会每轮重复整份背景。

## 2. 连续调度和缓存计划

每个 wave 同时启动 A–E 五个 burst；每个 burst 连续发送 3 轮：

- burst 第 1 → 2 轮：上一响应完成后随机等待 `90–180 秒`；
- burst 第 2 → 3 轮：上一响应完成后随机等待 `90–240 秒`；
- A–E 首轮启动再各自随机错开 `0–20 秒`。

10 个 wave 的计划如下：

| Wave | 每个会话轮次 | 首轮预期 | Wave 前等待 |
|---:|---:|---|---:|
| 1 | 1–3 | cold | 3–8 分钟 |
| 2 | 4–6 | hit | 1–2.5 分钟 |
| 3 | 7–9 | TTL miss | 6–10 分钟 |
| 4 | 10–12 | hit | 1–3 分钟 |
| 5 | 13–15 | TTL miss | 7–12 分钟 |
| 6 | 16–18 | hit | 1.5–3 分钟 |
| 7 | 19–21 | TTL miss | 6–10 分钟 |
| 8 | 22–24 | hit | 1–2.5 分钟 |
| 9 | 25–27 | TTL miss | 7–12 分钟 |
| 10 | 28–30 | hit | 1.5–3 分钟 |

计划类别总计：

- cold：5 个；
- TTL miss：20 个；
- hit：125 个。

实际响应时间、另一个并行会话的完成时间或用量暂停可能让计划 hit 超过 5 分钟。runtime 会在真正发送前根据该会话上一响应完成时间把它自动改记为 `ttl_miss`。

无用量暂停时，150 个请求通常会在约 1.5–3 小时内完成，因为五个会话是并行的；这只是估算，真实响应时长、限流和 5h 暂停会直接增加总耗时。

## 3. 会话身份

这次测试模拟“一个 Claude 账号、同一台客户端设备、五个终端会话”：

- A–E 共用一个 64 位十六进制 device ID；
- A–E 分别拥有五个不同的 UUID session ID；
- 同一会话的 30 个请求始终携带同一个 `metadata.user_id.session_id`；
- 每个新 run 生成新的 A–E session ID；
- device ID 保存在账号级目录中，跨验证、正式运行和重新测试复用。

严格来说 device ID 表示客户端安装/设备，并不等同于账号；但本测试只有一台服务器和一个账号，因此按账号 ID 持久化一个 device ID 最符合模拟目标。

metadata passthrough 必须保持关闭。下游 metadata 用于 sub2api 在选账号和检查 `max_sessions` 前识别稳定会话，不会原样发给 Anthropic；OAuth mimic 仍使用账号指纹身份构造上游 metadata 和稳定的 `x-claude-code-session-id`。

## 4. sub2api 账号配置

管理页面建议设置：

| 项目 | 值 |
|---|---:|
| 账号并发 | 5 |
| 5h 窗口费用控制 | 开启 |
| 费用阈值 | `$11.20`（仍需按实际 utilization 校准） |
| 粘性预留额度 | `$2.60`（仍需校准） |
| 会话数量控制 | 开启 |
| 最大会话数 | 5 |
| 会话空闲超时 | 60 分钟 |
| RPM 限制 | 开启 |
| 基础 RPM | 8 |
| RPM 策略 | 分层限流 |
| RPM 粘性缓冲 | 5 |
| 用户消息限速 | 软性限速 |

同时确认：

- `session_id_masking_enabled=false`；
- metadata passthrough 关闭；
- messages cache rewrite 关闭；
- cache TTL override 关闭，或明确设为 `5m`；
- 测试 API key 所属分组只包含这个 Claude 账号。

不要选择“串行队列”，它会按账号 ID 获取全局锁，把 A–E 五路重新排成串行。

旧运行留下的 session-limit 记录会保留到空闲超时。换成最大会话数 5 后，应先等待管理页面显示活动会话 `0/5`，再启动新 run；不要直接清空整个 Redis。

## 5. 本机提交与服务器更新

本机：

```bash
cd /Users/ling/sub2api
git status --short --branch
git add .gitignore docs/test/claude-oauth-soak-24h
git commit -m "test: run continuous five-session Claude soak"
git push myfork codex/claude-request-alignment
```

服务器：

```bash
cd /root/sub2api-src
git fetch origin
git checkout codex/claude-request-alignment
git pull --ff-only origin codex/claude-request-alignment

cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
chmod 700 *.sh sessions/*.sh
```

这些修改只涉及测试脚本和文档，不需要重新构建 sub2api 镜像。

### 5.1 每次修改后的重新部署与运行流程

这里要区分两个“服务”：

- sub2api 容器：只有修改了 Go 后端、Dockerfile 或运行配置时才需要重建或重启；
- 压测程序 `run_24h.sh`：只要修改了 prompt、followup、session 脚本、调度表或 runner，都必须停止旧 run，再用全新的 `SOAK_OUTPUT_DIR` 启动。

不要在压测运行过程中直接替换这些文件。runner 会在后续 wave 继续读取 `schedule.tsv` 和 followup，原地修改会让同一个 run 前后使用两套计划。

#### 第一步：先停止正在运行的旧压测

在服务器执行：

```bash
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* 2>/dev/null | head -n 1 || true)

if [[ -n $run_dir ]]; then
  touch "$run_dir/control/STOP"
  tail -n 30 "$run_dir/run.log"
fi

pgrep -af '[r]un_24h.sh' || echo '没有正在运行的压测程序'
```

`STOP` 是优雅停止：不再创建新请求，已经在途的请求可以完成。若确认必须立即中断，再进入原 tmux 按 `Ctrl-C`。更新文件或重启 sub2api 前，应确认 `pgrep` 已经没有旧 runner。

#### 第二步：在本机提交并推送改动

```bash
cd /Users/ling/sub2api
git status --short --branch

# 只暂存本次压测包；不要把抓包、密钥或无关文件一起提交
git add docs/test/claude-oauth-soak-24h
git diff --cached --check
git commit -m "test: update Claude OAuth soak scenarios"
git push myfork codex/claude-request-alignment
```

#### 第三步：服务器拉取最新版本

```bash
cd /root/sub2api-src
git fetch origin
git checkout codex/claude-request-alignment
git pull --ff-only origin codex/claude-request-alignment

cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
chmod 700 *.sh sessions/*.sh
```

#### 第四步：判断是否需要重启 sub2api

如果只改了以下内容，不要重建或重启 sub2api：

- `docs/test/claude-oauth-soak-24h/prompts/*`；
- `followups.json`；
- `sessions/*.sh`；
- `schedule.tsv`、`run_24h.sh`、`send_turn.sh`、`lib/runtime.sh`；
- 本 README。

这些都是下游 curl 压测客户端文件，拉取后重新启动 runner 即可。

如果改了 sub2api 的 Go 转发、粘性会话、OAuth mimic、缓存或用量采集代码，则重新构建并只重建应用容器：

```bash
cd /root/sub2api-src
docker build -t weishaw/sub2api:latest .

cd /root/sub2api-deploy
docker compose up -d --no-deps --force-recreate --pull never sub2api
docker compose ps
curl -fsS http://127.0.0.1:8080/health
docker compose logs --tail=100 sub2api
```

这个操作不会重建 PostgreSQL 和 Redis，也不会覆盖 `/root/sub2api-deploy/.env` 或现有数据。

如果只修改了 `/root/sub2api-deploy/.env` 或 Compose 中 sub2api 的环境配置，不需要重新 build 镜像，但需要让应用容器重新创建以读取新配置：

```bash
cd /root/sub2api-deploy
docker compose up -d --no-deps --force-recreate --pull never sub2api
curl -fsS http://127.0.0.1:8080/health
```

#### 第五步：每次都先做静态检查和无网络校验

确保已经按第 7 节设置好环境变量，然后执行：

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h

bash -n run_24h.sh send_turn.sh usage_guard.sh \
  init_session_identities.sh verify_session_identities.sh \
  summarize.sh lib/runtime.sh sessions/*.sh

jq -e 'all(.[]; length == 29)' prompts/followups.json >/dev/null
./usage_guard.sh

export SOAK_OUTPUT_DIR="/root/sub2api-soak-validation/validate-$(date -u +%Y%m%dT%H%M%SZ)"
export SOAK_VALIDATE_ONLY=1
./run_24h.sh
./verify_session_identities.sh "$SOAK_OUTPUT_DIR"
unset SOAK_VALIDATE_ONLY
```

校验不发送业务请求。它必须显示当前预期的会话数、每会话 30 轮和身份数量；如果仍显示旧数量，不要开始正式测试。

#### 第六步：使用新目录重新运行

如果是新 SSH 或新 tmux，先完整执行第 7 节的环境变量配置。然后确认旧活动会话已经按空闲超时归零，再运行：

```bash
export SOAK_OUTPUT_DIR="/root/sub2api-soak-runs/run-$(date -u +%Y%m%dT%H%M%SZ)"
printf 'formal output: %s\n' "$SOAK_OUTPUT_DIR"
./run_24h.sh
```

不能复用旧 run 目录，也不要复制旧 `state/*.messages.json` 或旧 `session-identities.json`。账号级 `SOAK_IDENTITY_HOME` 应继续复用，因此 device ID 保持不变；新 run 会自动为所有计划会话生成新的 session ID。

### 5.2 新增会话时的检查清单

例如以后新增 `session-f`，需要同时完成：

1. 新增长首轮 prompt 文件，确保长度足以形成缓存前缀；
2. 在 `prompts/followups.json` 增加 `session-f`，且恰好包含 29 个追问；
3. 新增可执行的 `sessions/session-f.sh`，引用正确的 prompt；
4. 在 `schedule.tsv` 的每个 wave 增加一行，共覆盖首轮 `1、4、7……28`；
5. 更新 `run_24h.sh` 的 `planned_session_count`，`planned_request_count` 会自动重新计算；
6. 相应提高 sub2api 的账号并发、最大会话数和 RPM 粘性缓冲；
7. 更新 README 的会话数、总请求数和缓存预期统计；
8. 按 5.1 节停止旧 run、拉取、校验并用新目录运行。

身份初始化和身份验证会从 `schedule.tsv` 自动发现新增会话，不需要手工填写 session UUID。

## 6. 服务器依赖与账号 ID

```bash
cd /root/sub2api-deploy
docker compose ps
curl -fsS http://127.0.0.1:8080/health

command -v curl
command -v jq
command -v flock
command -v od
command -v tmux
```

并行模式必须有 `flock`。查询 Claude 账号数据库 ID：

```bash
docker compose exec -T postgres sh -c \
  'psql -X -A -F "|" -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c \
  "SELECT id,name,platform,type,status,schedulable FROM accounts WHERE deleted_at IS NULL ORDER BY id;"'
```

## 7. 在 tmux 中设置环境

```bash
tmux new -s sub2api-soak
```

进入 tmux 后执行：

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h

export SOAK_BASE_URL='http://127.0.0.1:8080'
read -rsp 'sub2api test API key: ' SOAK_API_KEY
printf '\n'
export SOAK_API_KEY

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

# 0 表示不使用本地请求数上限，只由 utilization 和服务器配置保护
export SOAK_MAX_REQUESTS_PER_5H='0'

unset SOAK_VALIDATE_ONLY SOAK_DRY_RUN SOAK_SKIP_WAITS \
  SOAK_TEST_MODE SOAK_AUTO_CONTINUE \
  SOAK_SESSION_IDENTITIES_FILE
```

`SOAK_PARALLEL_SESSIONS=1` 是“启用并行”的布尔开关，不表示只运行 1 个会话；实际并行的五个会话来自 `schedule.tsv` 同一个 wave 的五行。

API key 是调用 sub2api 的下游 `sk-...`，不是 Claude OAuth token。输入时没有回显是正常的。

检查变量但不显示密钥：

```bash
printf 'BASE_URL=%s ACCOUNT_ID=%s API_KEY_LENGTH=%s\n' \
  "$SOAK_BASE_URL" "$SOAK_ACCOUNT_ID" "${#SOAK_API_KEY}"
```

## 8. 启动前验证

先读取数据库被动用量样本：

```bash
./usage_guard.sh
```

输出格式：

```text
5h百分比|7d百分比|5h重置Unix时间|上游采样时间|样本年龄秒数
```

再执行无网络校验：

```bash
export SOAK_OUTPUT_DIR="/root/sub2api-soak-validation/validate-$(date -u +%Y%m%dT%H%M%SZ)"
export SOAK_VALIDATE_ONLY=1

./run_24h.sh
./verify_session_identities.sh "$SOAK_OUTPUT_DIR"

unset SOAK_VALIDATE_ONLY
```

应看到：

```text
schedule validated: 5 sessions x 30 turns in 10 continuous parallel waves
session identities ready: ... sessions=5
SOAK_VALIDATE_ONLY=1; schedule and identities validated without sending requests
verified unique identities and 0 request bodies
```

校验目录已经非空，不能作为正式输出目录。

## 9. 正式启动

确认旧活动会话已经归零，然后创建全新的输出目录：

```bash
export SOAK_OUTPUT_DIR="/root/sub2api-soak-runs/run-$(date -u +%Y%m%dT%H%M%SZ)"
printf 'formal output: %s\n' "$SOAK_OUTPUT_DIR"

./run_24h.sh
```

启动后第一组请求会先随机等待 3–8 分钟。之后不会出现固定 5 小时静默，也不需要创建 `continue-phase-*` 文件。

从 tmux 脱离但保持运行：按 `Ctrl-b`，松开后按 `d`。重新进入：

```bash
tmux attach -t sub2api-soak
```

不要对同一个 `SOAK_OUTPUT_DIR` 再次运行 `run_24h.sh`。

## 10. 用量保护行为

在 `required` 模式下，请求前、随机等待后和成功响应后都会读取 PostgreSQL 中账号最近一次上游响应保存的：

- `session_window_utilization`；
- `passive_usage_7d_utilization`；
- `session_window_end`；
- `passive_usage_sampled_at`。

行为如下：

- 5h 达到 `78%`：写入 `PAUSED_ON_5H_LIMIT`，等待 reset 时间加 `60–180 秒`，随后重新检查并继续剩余 wave；
- 7d 达到 `70%`：写入 `STOPPED_ON_7D_LIMIT` 和 `STOP`，整个运行结束；
- 查询失败、账号不存在或 bootstrap 后仍没有样本：fail-closed；
- 任意非 2xx 或异常响应：不推进会话状态，停止整个运行且不自动重试；
- 24 小时 deadline 到达：停止剩余请求；
- 150 个请求先完成：立即正常结束，不等待到 24 小时。

最多五个请求可能同时在途。`78%` 是当前目标值，但它不是严格的 `80%` 硬墙：五路在同一份略有滞后的 usage 样本下都可能已经放行。若首个 5h 窗口观察到单个 wave 会让 utilization 跳升超过 2%，后续运行应把 `SOAK_USAGE_5H_STOP_PERCENT` 下调到 `75`；自动保护无法撤回已经发出的请求。

`SOAK_MAX_REQUESTS_PER_5H=0` 表示禁用旧的本地请求数量上限。成功时间戳和在途 reservation 仍会记录，用于审计，但不会因为达到 18 条而暂停。如果将其设置为正整数，才会恢复额外的滚动请求数保护。

数据库数据来自最近一次经过 sub2api 的响应，不是独立实时探针。如果同一 Claude 账号还在其他地方使用，仍应人工观察官方 Usage 页面。

## 11. cache_control 和历史

每个请求发送完整的本会话历史，并只在当前最新 user text block 上添加：

```json
{"cache_control":{"type":"ephemeral","ttl":"5m"}}
```

状态文件不保留旧 cache_control。成功响应的原始 assistant `content` 会完整保存，因此 thinking、signature、text 或 tool block 不会被强行拼成纯文本。失败请求不会推进状态。

每个 run 的关键状态：

```text
session-identities.json
state/session-a.messages.json
state/session-b.messages.json
state/session-c.messages.json
state/session-d.messages.json
state/session-e.messages.json
```

预计 hit 并不保证实际命中。判断长项目前缀是否命中，主要看：

- `cache_read_input_tokens` 是否达到项目长前缀量级；
- TTL miss/cold 是否出现相应的 `cache_creation_input_tokens`；
- 不要只用 `input_tokens` 是否为 1 判断。

## 12. 监控

另开 SSH 终端：

```bash
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* | head -n 1)
tail -F "$run_dir/run.log"
```

查看 sub2api 日志：

```bash
cd /root/sub2api-deploy
docker compose logs --tail=100 -f sub2api
```

查看上游请求快照：

```bash
tail -F /root/sub2api-deploy/data/gateway_debug.log
```

验证所有已生成请求的下游身份：

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
./verify_session_identities.sh "$run_dir"
```

调度日志检查：

```bash
cd /root/sub2api-deploy
docker compose logs --since 30m sub2api 2>&1 |
  grep -E 'sticky.hash_source|sticky.session_hash_generated'
```

应看到 `source=metadata_user_id`；A–E 的 UUID 各自跨轮稳定且彼此不同。上游快照中各会话自己的 `x-claude-code-session-id` 也应稳定。

本方案不显式发送 `stream`，正常不应触发当前实验性的 quota/title 伴生请求。

## 13. 手动停止

优雅停止：

```bash
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* | head -n 1)
touch "$run_dir/control/STOP"
```

脚本最长约 30 秒发现 STOP，不再发送新请求；已经在途的最多五个 curl 会等待响应完成或在 900 秒超时。

立即中断 tmux 中的在途请求：

```bash
tmux send-keys -t sub2api-soak C-c
touch "$run_dir/control/STOP"
```

确认：

```bash
tail -n 30 "$run_dir/run.log"
pgrep -af '[r]un_24h.sh' || echo '压测脚本已停止'
```

## 14. 输出与汇总

每个正式 run 包含：

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

汇总：

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h
./summarize.sh "$run_dir"
column -t -s $'\t' "$run_dir/manifest.tsv" | less -S
find "$run_dir/control" -maxdepth 1 -type f -print
```

## 15. 通过标准

- 最多五个不同会话请求并行，同一会话从不并发修改历史；
- A–E 各完成 30 轮，或明确因为 5h/7d/deadline/错误保护停止；
- A–E 的下游 metadata session ID 各自稳定且互不相同；
- A–E 的上游 session ID 各自稳定；
- 计划 hit 大多数出现长前缀量级 cache read；
- 计划 TTL miss/cold 大多数出现 cache creation；
- 没有无法解释的 400、401、403、429 或 5xx；
- sub2api、Redis、PostgreSQL 持续健康；
- 5h/7d 没有越过设定的安全目标；
- 每个请求都能由 schedule、manifest、请求快照和响应解释。

提示词回答质量不是本轮主要通过标准；重点是请求链路、身份、缓存、调度和保护行为。
