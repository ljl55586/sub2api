# Claude OAuth 单账号单会话逐请求确认压测

以后每次修改源码或压测脚本时，请先阅读独立操作手册：[sub2api 源码同步、重新部署与压测运行手册](./服务器同步文档.md)。

本测试包只通过 `curl` 调用 sub2api `/v1/messages`，用于观察单个 Claude OAuth/Pro 账号的连续多轮、粘性会话、Claude Code 1 小时提示缓存、会话启动伴生请求和用量保护。

当前正式计划只运行一个新会话：

- Session F：Go + PostgreSQL + Redis 多租户 Webhook 投递平台，从事件写入、调度、lease、重试和租户顺序逐步讨论到 SSRF、密钥轮换、回放、容量、事故修复和发布。

Session F 共 30 个主请求，严格串行。首轮还会按 quota → title → main 的顺序发送一次 quota 和一次 title；如果伴生请求首次成功，完整运行预计产生 32 个上游请求。

正式 runner 会先用 `curl` 把下游请求交给 sub2api，但 sub2api 会在真正访问 Anthropic 之前暂停。首轮完成所有改写、缓存断点、CCH 和 header 构造后，把 `quota + title + main` 三个最终上游请求一起展示，只确认一次；后续每轮只展示最终 `main`，每条分别确认。只有输入精确字符串 `SEND REQUEST`，后端才会放行同一批已经构造好的请求对象。输入其他内容、关闭输入、创建 `STOP` 文件或超过 24 小时截止时间都会写入 reject，Anthropic 不会收到该 stage。

调度器不再划分固定的 5 个 phase，也没有“每个 phase 18 个请求”或“滚动 5h 最多 18 个请求”的默认限制。它连续执行 10 个 wave，真正控制发送的是上游用量保护：

- 5h utilization 达到 `78%`：停止发送，等待当前窗口重置后自动继续剩余请求；
- 7d utilization 达到 `70%`：终止整个运行；
- 30 个计划请求全部完成但 5h 仍低于阈值：正常结束，不重复旧问题，也不自动增加请求。

## 1. 文件和会话设计

```text
claude-oauth-soak-24h/
├── README.md
├── cache_hit_demo.sh
├── run_24h.sh
├── send_turn.sh
├── parse_anthropic_sse.sh
├── test_cache_hit_demo.sh
├── test_run_24h.sh
├── test_send_turn.sh
├── testdata/
│   ├── fake_curl.sh
│   └── valid-response.json
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
│   ├── session-e.sh
│   └── session-f.sh
└── prompts/
    ├── session-a-go-log-analyzer.md
    ├── session-b-booking-service.md
    ├── session-c-redis-delay-queue.md
    ├── session-d-payment-reconciliation.md
    ├── session-e-data-import-pipeline.md
    ├── session-f-webhook-delivery.md
    ├── cache-demo-followup.md
    └── followups.json
```

### Session F：多租户 Webhook 投递平台

首轮是一份约 24.8KB 的完整项目审查材料，包含：

- PostgreSQL schema、事件事务写入和 scheduler 查询；
- Go worker、claim/lease、HTTP 投递、重试和死信处理；
- Redis 租户并发与顺序锁；
- pause、cancel、replay、secret rotation 和 SSRF 防护；
- 指标、日志、故障案例、现有测试和部署约束。

后续 29 轮都沿同一个项目自然推进，覆盖重复投递、提交结果未知、lease fencing、租户内顺序、限流公平性、DNS rebinding、签名密钥轮换、历史回放、分区迁移、容量规划、故障演练和最终发布检查。长背景只在首轮发送一次，后续依靠完整的 user/assistant 历史形成真实缓存前缀。

Session A–E 的旧脚本和材料仍保留在目录中，便于以后复用，但当前 `schedule.tsv` 不会运行它们。

## 2. 连续调度和缓存计划

每个 wave 只运行 Session F 的一个 burst；每个 burst 连续发送 3 轮：

- burst 第 1 → 2 轮：上一响应完成后随机等待 `90–180 秒`；
- burst 第 2 → 3 轮：上一响应完成后随机等待 `90–240 秒`；
- 第 1 轮不预先等待，启动后立即构造并展示请求 JSON。

10 个 wave 的计划如下：

| Wave | 每个会话轮次 | 首轮预期 | Wave 前等待 |
|---:|---:|---|---:|
| 1 | 1–3 | cold | 0 |
| 2 | 4–6 | hit | 1–2.5 分钟 |
| 3 | 7–9 | TTL miss | 65–75 分钟 |
| 4 | 10–12 | hit | 1–3 分钟 |
| 5 | 13–15 | TTL miss | 65–75 分钟 |
| 6 | 16–18 | hit | 1.5–3 分钟 |
| 7 | 19–21 | TTL miss | 65–75 分钟 |
| 8 | 22–24 | hit | 1–2.5 分钟 |
| 9 | 25–27 | TTL miss | 65–75 分钟 |
| 10 | 28–30 | hit | 1.5–3 分钟 |

计划类别总计：

- cold：1 个；
- TTL miss：4 个；
- hit：25 个。

流式 OAuth no-tools 画像会把上游缓存断点设为 `1h`。实际响应时间、人工确认时间或用量暂停可能让计划 hit 超过 1 小时；runtime 会在请求展示前检查一次，`send_turn.sh` 还会在确认后、发送前再次检查，并把超时的计划 hit 改记为 `ttl_miss`。请求 JSON 本身不会因此改变。

无用量暂停时，四次主动 TTL 过期等待本身就需要约 4.3–5 小时，完整运行通常需要约 5–8 小时。真实响应时长、限流和 5h 暂停会继续增加总耗时。

## 3. 会话身份

这次测试模拟“一个 Claude 账号、同一台客户端设备、一个终端会话”：

- Session F 使用一个 64 位十六进制 device ID；
- Session F 使用一个独立的 UUID session ID；
- 同一会话的 30 个请求始终携带同一个 `metadata.user_id.session_id`；
- 每个新 run 生成新的 Session F session ID；
- device ID 保存在账号级目录中，跨验证、正式运行和重新测试复用。

严格来说 device ID 表示客户端安装/设备，并不等同于账号；但本测试只有一台服务器和一个账号，因此按账号 ID 持久化一个 device ID 最符合模拟目标。

metadata passthrough 必须保持关闭。下游 metadata 用于 sub2api 在选账号和检查 `max_sessions` 前识别稳定会话，不会原样发给 Anthropic；OAuth mimic 仍使用账号指纹身份构造上游 metadata 和稳定的 `x-claude-code-session-id`。

### 会话启动顺序

Session F 的首轮使用 `stream:true`。它第一次命中所选 OAuth 账号时，网关会先完成 quota/title/main 的最终 body/header 构造，并把三个请求一起停在上游 approval gate：

```text
同一 preview、一次 SEND REQUEST
              │
              ▼
quota ──> 收到响应 headers 或明确的传输错误
              │
              ▼
       随机等待 45–90 秒
              │
              ▼
title ──> 收到响应 headers 或明确的传输错误
              │
              ▼
             main
```

确认前，三个请求都没有调用上游 transport。确认后，后端严格执行上图顺序：quota 没到响应/错误边界时 title 不能发送，title 没到响应/错误边界时 main 不能发送。quota 或 title 失败时仍会继续下一步，避免 main 永久卡住。这个严格顺序只用于带正确 approval token 的压测请求；普通 API 请求不能利用私有 delay header 拖住 main。quota、title、main 使用相同的最终 `metadata.user_id` 和 `x-claude-code-session-id`，但 quota/title 的 messages 和响应不会拼进 main 历史。成功的伴生 action 状态保留在 30 小时 runtime 中。main 的自动重试会形成新的 main-only approval stage；如果 quota/title 发送失败并在后续轮次重新出现，严格 runner 会因 bundle 不再是 main-only 而 reject 并停止，要求先调查失败原因。

`send_turn.sh` 默认使用 `stream:false`，不会因为没有设置环境变量就意外触发伴生请求。本方案只有在第 7 节显式执行 `export SOAK_STREAM='true'` 后才使用上述启动流程；正式启动前务必检查该变量。

## 4. sub2api 账号配置

管理页面建议设置：

| 项目 | 值 |
|---|---:|
| 账号并发 | 1 |
| 5h 窗口费用控制 | 开启 |
| 费用阈值 | `$11.20`（仍需按实际 utilization 校准） |
| 粘性预留额度 | `$2.60`（仍需校准） |
| 会话数量控制 | 开启 |
| 最大会话数 | 1 |
| 会话空闲超时 | 60 分钟 |
| RPM 限制 | 开启 |
| 基础 RPM | 4 |
| RPM 策略 | 分层限流 |
| RPM 粘性缓冲 | 1 |
| 用户消息限速 | 软性限速 |

同时确认：

- `session_id_masking_enabled=false`；
- metadata passthrough 关闭；
- messages cache rewrite 关闭；
- cache TTL override 关闭，让 `stream:true` Claude OAuth no-tools 画像使用真实 CLI 的 `1h`；
- 测试 API key 所属分组只包含这个 Claude 账号。

当前只有一个主请求在途，因此无需用“串行队列”额外限制并发。

旧运行留下的 session-limit 记录会保留到空闲超时。换成最大会话数 1 后，应先等待管理页面显示活动会话 `0/1`，再启动新 run；不要直接清空整个 Redis。

## 5. 本机提交与服务器更新

本机：

```bash
cd /Users/ling/sub2api
git status --short --branch
git add \
  backend/internal/service/gateway_claude_oauth_companion.go \
  backend/internal/service/gateway_forward.go \
  backend/internal/service/gateway_service.go \
  backend/internal/service/gateway_claude_upstream_approval.go \
  backend/internal/service/gateway_claude_upstream_approval_test.go \
  deploy/docker-compose.yml \
  deploy/docker-compose.local.yml \
  deploy/docker-compose.dev.yml \
  deploy/docker-compose.standalone.yml \
  docs/test/claude-oauth-soak-24h
git commit -m "test: approve final Claude upstream requests"
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

本次同时修改了 Go 网关和压测脚本。服务器拉取后必须按下方第 5.1 节重新构建 sub2api 镜像并重建应用容器，然后才能启动新的压测 run。

### 5.0 配置最终上游请求 approval gate

此功能默认关闭。正式压测前，在 `/root/sub2api-deploy/.env` 中设置一对只用于本机压测的配置：

```bash
cd /root/sub2api-deploy
install -d -m 700 data/upstream-approval

approval_token=$(openssl rand -hex 32)
printf 'SUB2API_UPSTREAM_APPROVAL_DIR=/app/data/upstream-approval\n' >>.env
printf 'SUB2API_UPSTREAM_APPROVAL_TOKEN=%s\n' "$approval_token" >>.env
unset approval_token
```

如果 `.env` 已经有这两个键，应编辑原值，不能追加重复键。Compose 必须把 `./data` bind mount 到容器的 `/app/data`；可用以下命令确认：

```bash
docker compose config | sed -n '/sub2api:/,/^[^ ]/p' | grep -A4 -B2 '/app/data'
```

`/root/sub2api-deploy/docker-compose.yml` 中 sub2api 服务的 `environment` 还必须包含以下传递项（源码仓库的四份 Compose 模板已更新，但既有部署目录不会自动被覆盖）：

```yaml
- SUB2API_UPSTREAM_APPROVAL_DIR=${SUB2API_UPSTREAM_APPROVAL_DIR:-}
- SUB2API_UPSTREAM_APPROVAL_TOKEN=${SUB2API_UPSTREAM_APPROVAL_TOKEN:-}
```

重建容器后确认配置进入了容器，但不要打印 token：

```bash
docker compose exec -T sub2api sh -c '
  set -e
  test "$SUB2API_UPSTREAM_APPROVAL_DIR" = /app/data/upstream-approval
  test -n "$SUB2API_UPSTREAM_APPROVAL_TOKEN"
'
```

正式 runner 读取的宿主机目录是 `/root/sub2api-deploy/data/upstream-approval`，后端使用的容器内目录是 `/app/data/upstream-approval`，二者必须是同一个 bind mount。命名 volume 部署不能直接使用这个 runner 路径，需先改为明确的宿主机 bind mount。

approval token 只用于授权“把已暂停的最终上游请求写入共享目录并等待放行”，不能使用 sub2api API key 或 Claude OAuth token 代替。preview 中的 `Authorization` 和 `x-api-key` 会脱敏，token 本身也不会写入 preview。

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

# 只暂存本次 gate、Compose 模板和压测包；不要把抓包、密钥或无关文件一起提交
git add \
  backend/internal/service/gateway_claude_oauth_companion.go \
  backend/internal/service/gateway_forward.go \
  backend/internal/service/gateway_service.go \
  backend/internal/service/gateway_claude_upstream_approval.go \
  backend/internal/service/gateway_claude_upstream_approval_test.go \
  deploy/docker-compose*.yml \
  docs/test/claude-oauth-soak-24h
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

bash -n run_24h.sh send_turn.sh parse_anthropic_sse.sh usage_guard.sh \
  init_session_identities.sh verify_session_identities.sh \
  summarize.sh test_cache_hit_demo.sh test_run_24h.sh test_send_turn.sh \
  lib/runtime.sh sessions/*.sh

jq -e 'all(.[]; length == 29)' prompts/followups.json >/dev/null
./test_send_turn.sh
./test_cache_hit_demo.sh
./test_run_24h.sh
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

例如以后新增 `session-g`，需要同时完成：

1. 新增长首轮 prompt 文件，确保长度足以形成缓存前缀；
2. 在 `prompts/followups.json` 增加 `session-g`，且恰好包含 29 个追问；
3. 新增可执行的 `sessions/session-g.sh`，引用正确的 prompt；
4. 在 `schedule.tsv` 的每个 wave 增加一行，共覆盖首轮 `1、4、7……28`；
5. 更新 `run_24h.sh` 的 `planned_session_count`，`planned_request_count` 会自动重新计算；
6. 如果要与 Session F 并行运行，再相应提高 sub2api 的账号并发、最大会话数和 RPM 粘性缓冲；
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
command -v od
command -v less
command -v openssl
command -v tmux
```

当前单会话正式 runner 不需要 `flock`。查询 Claude 账号数据库 ID：

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
# 默认不下发 max_tokens；当前 Claude OAuth 转发会给 Opus 4.8 上游补成 64000。
# 如果当前 shell 曾按旧教程设置过 1536，这里必须显式清除。
unset SOAK_MAX_TOKENS
export SOAK_USER_AGENT='soak-curl/1.0'
export SOAK_STREAM='true'
export SOAK_CACHE_TTL_SECONDS='3600'
# 首轮 quota 收到上游响应 headers（或明确失败）后，随机等待多久再发 title。
# runner 默认就是 45–90 秒；这里显式写出，便于每次启动前核对。
export SOAK_COMPANION_DELAY_MIN_SECONDS='45'
export SOAK_COMPANION_DELAY_MAX_SECONDS='90'
# 0 表示最终上游 stage 展示后无限等待确认
export SOAK_CONFIRM_TIMEOUT_SECONDS='0'
export SOAK_UPSTREAM_APPROVAL_DIR='/root/sub2api-deploy/data/upstream-approval'
SOAK_UPSTREAM_APPROVAL_TOKEN=$(
  awk -F= '$1 == "SUB2API_UPSTREAM_APPROVAL_TOKEN" {
    print substr($0, index($0, "=") + 1)
  }' /root/sub2api-deploy/.env | tail -n 1
)
export SOAK_UPSTREAM_APPROVAL_TOKEN
export SOAK_UPSTREAM_PREVIEW_PAGER='auto'

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
  SOAK_SESSION_IDENTITIES_FILE SOAK_PARALLEL_SESSIONS \
  SOAK_CONFIRM_BEFORE_SEND
```

正式 `run_24h.sh` 会强制设置单会话、前台串行和最终上游 stage 确认；即使当前 shell 留有旧的并行变量，也不会并发启动主请求。

API key 是调用 sub2api 的下游 `sk-...`，不是 Claude OAuth token。approval token 是另一段本机共享密钥；两者都不会显示在 preview 中。

检查变量但不显示密钥：

```bash
printf 'BASE_URL=%s ACCOUNT_ID=%s DELAY=%s-%ss API_KEY_LENGTH=%s APPROVAL_DIR=%s APPROVAL_TOKEN_LENGTH=%s\n' \
  "$SOAK_BASE_URL" "$SOAK_ACCOUNT_ID" \
  "$SOAK_COMPANION_DELAY_MIN_SECONDS" "$SOAK_COMPANION_DELAY_MAX_SECONDS" \
  "${#SOAK_API_KEY}" \
  "$SOAK_UPSTREAM_APPROVAL_DIR" "${#SOAK_UPSTREAM_APPROVAL_TOKEN}"
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
schedule validated: 1 session x 30 turns in 10 confirmed waves
session identities ready: ... sessions=1
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

启动后，runner 先把下游请求提交给 sub2api；sub2api 完成最终上游构造后暂停。首轮终端会显示：

```text
========== FINAL UPSTREAM REQUEST(S): NOT SENT ==========
bundle: quota,title,main
approved send order: quota -> wait 67s -> title -> main
...
Type exactly SEND REQUEST to release this upstream stage:
```

其中 `67s` 只是本轮示例。runner 会在配置的 `45–90` 秒范围内为每个新会话随机选择一次，并把实际值同时写入请求 header、终端提示和 `manifest.tsv`。

交互终端默认用 `less` 打开完整 preview，方向键、PageUp/PageDown 可以上下浏览，按 `q` 返回确认提示。preview 包含最终 URL、脱敏 headers、完整 body、body SHA-256 和 `network_sent:false`。确认后在同一终端输入：

```text
SEND REQUEST
```

只有这条精确字符串会让后端开始调用 Anthropic transport。首轮的 quota/title/main 只确认一次，但确认后由后端按 quota → 延迟 → title → main 依次发送；后续 29 条 main 各确认一次，因此正常完成仍需输入 30 次。输入其他内容或关闭 stdin 会以退出码 `75` 停止整个 run，preview 保留在 `upstream-previews/`，后端收到 `.reject`，不会访问 Anthropic，也不会推进 `state/`。应使用新 `SOAK_OUTPUT_DIR` 修正后重跑，不能在旧目录继续。

首轮批准后，quota 会立即发送；quota 收到响应 headers 或明确传输错误后，后端等待本轮随机秒数再发送 title；title 收到响应 headers 或明确传输错误后才发送 main。之后不会出现固定 5 小时静默，也不需要创建 `continue-phase-*` 文件。

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
- `stop_reason=max_tokens`、非 `end_turn` 或没有非空 text block：即使 HTTP 200 也按失败处理，不推进状态并停止整个运行；
- 24 小时 deadline 到达：停止剩余请求；
- 30 个请求先完成：立即正常结束，不等待到 24 小时。

最多只有一个主请求在途。`78%` 是当前目标值，但数据库样本来自上一条经过 sub2api 的响应，仍可能略有滞后；人工确认前应同时观察官方 Usage 页面。若单个请求会让 utilization 跳升超过预期，后续运行应把 `SOAK_USAGE_5H_STOP_PERCENT` 下调到 `75`；自动保护无法撤回已经发出的请求。

`SOAK_MAX_REQUESTS_PER_5H=0` 表示禁用旧的本地请求数量上限。成功时间戳和在途 reservation 仍会记录，用于审计，但不会因为达到 18 条而暂停。如果将其设置为正整数，才会恢复额外的滚动请求数保护。

数据库数据来自最近一次经过 sub2api 的响应，不是独立实时探针。如果同一 Claude 账号还在其他地方使用，仍应人工观察官方 Usage 页面。

## 11. cache_control 和历史

每个主请求使用 `stream:true` 并发送完整的本会话历史；curl 收到的原始 SSE 保存在 `responses/*.raw`，`parse_anthropic_sse.sh` 将其重组成 `responses/*.json`，再用于 token 统计和下一轮 assistant 历史。下游只在当前最新 user text block 上添加：

```json
{"cache_control":{"type":"ephemeral","ttl":"5m"}}
```

流式 OAuth no-tools 画像会在最终上游请求中把活动断点调整成 `1h`。状态文件不保留旧 cache_control。只有 `stop_reason=end_turn` 且包含非空 text block 的响应才会推进状态；通过校验后，重组出的 assistant `content` 会完整保存，因此合法的 thinking、signature 和 text block 不会被强行拼成纯文本。`max_tokens` 截断、纯 thinking、残缺 SSE 或其他非完整响应只保留 raw response、headers 和 manifest 审计文件，不写入状态。

默认不在 curl 请求中发送 `max_tokens`，当前分支的 Claude OAuth 转发逻辑会给 Opus 4.8 上游请求补成 `64000`。如需显式限制，可以设置正整数 `SOAK_MAX_TOKENS`；不建议再使用 `1536`，复杂项目问题可能在内部思考阶段就耗尽该预算。

每个 run 的关键状态：

```text
session-identities.json
state/session-f.messages.json
```

预计 hit 并不保证实际命中。判断长项目前缀是否命中，主要看：

- `cache_read_input_tokens` 是否达到项目长前缀量级；
- TTL miss/cold 是否出现相应的 `cache_creation_input_tokens`；
- 不要只用 `input_tokens` 是否为 1 判断。

## 12. 两请求缓存确认 Demo

`cache_hit_demo.sh` 与 24h runner 独立。它只使用一个 `cache-demo` 会话，并严格执行：

1. 用超过 16KB 的支付对账项目包发送请求 1，最新 user block 带 `cache_control: 5m`；
2. 请求 1 必须是带非空 text 的 `end_turn`，且 usage 必须出现 cache creation 或 cache read；
3. 将请求 1 的完整 user/assistant 历史与第二个自然追问拼成请求 2；
4. 把请求 2 的完整 JSON 保存到 `requests/` 并打印到终端，此时尚未调用 curl；
5. 最多等待 240 秒，只有输入精确字符串 `SEND REQUEST` 才发送刚才展示的同一个文件；
6. 输出请求 2 的 `cache_creation_input_tokens` 和 `cache_read_input_tokens`，后者大于 0 才报告命中。

该 Demo 显式使用 `SOAK_STREAM=false`，因此不触发 quota/title，也继续验证原来的 5 分钟缓存流程。

运行前设置与正式压测相同的基本变量，但使用单独的新目录：

```bash
cd /root/sub2api-src/docs/test/claude-oauth-soak-24h

export SOAK_BASE_URL='http://127.0.0.1:8080'
read -rsp 'sub2api test API key: ' SOAK_API_KEY
printf '\n'
export SOAK_API_KEY

export SOAK_ACCOUNT_ID='1'
export SOAK_IDENTITY_HOME='/root/sub2api-soak-identity'
export CACHE_DEMO_OUTPUT_DIR="/root/sub2api-cache-demo/run-$(date -u +%Y%m%dT%H%M%SZ)"

unset SOAK_MAX_TOKENS
./cache_hit_demo.sh
```

请求 2 展示后，检查终端内容或另开 SSH 查看文件：

```bash
jq . "$CACHE_DEMO_OUTPUT_DIR"/requests/*-cache-demo-t2-*.json
```

确认无误后，在原终端输入：

```text
SEND REQUEST
```

输入其他内容、关闭 stdin 或 240 秒内没有确认，脚本都会以退出码 `75` 结束，并且不会发送请求 2。由于缓存测试使用 5 分钟 TTL，超时后应使用全新的 `CACHE_DEMO_OUTPUT_DIR` 重跑整个 Demo，不要直接发送已过期的第二请求。Demo 会主动清除继承的 `SOAK_MAX_TOKENS`，所以两个下游请求都不包含该字段，由 sub2api 在 OAuth 上游请求中补齐。

## 13. 监控

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

查看已经展示给 runner 的最终上游 preview：

```bash
find /root/sub2api-soak-runs -path '*/upstream-previews/*.preview.json' \
  -type f -print | sort | tail
```

这些 preview 是 approval 时实际暂停的最终请求；除认证值脱敏外，URL、headers 和 body 与批准后交给 transport 的同一请求对象一致。`network_sent:false` 表示生成 preview 时尚未访问上游。批准后的真实发送行为查看容器运行日志：

```bash
cd /root/sub2api-deploy
docker compose logs -f sub2api 2>&1 |
  grep -E 'Claude OAuth (quota|title|main|session startup)'
```

同一个 `session_id` 应在输入首轮 `SEND REQUEST` 后才观察到三条 `send start`，且顺序必须是 quota → title → main。title 的 `send start` 应比 quota 晚本轮显示的随机秒数；main 的 `send start` 必须在 title 已收到响应 headers 或明确传输错误后出现。由于后端在收到 headers 时就放行下一步，quota/title 的“完整响应处理完成”日志仍可能晚于后一个请求的 `send start`，这不表示发送顺序失效。

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

应看到 `source=metadata_user_id`；Session F 的 UUID 应跨 30 轮保持稳定，上游快照中的 `x-claude-code-session-id` 也应稳定。

本方案显式发送 `stream:true`。每个新 session 的 quota/title 成功后不应再次发送；若伴生请求失败，runtime 会保留可重试状态，后续轮次可能再次尝试。重复发送时先检查此前同一 `session_id` 的完成或失败日志，再判断是否异常。

## 14. 手动停止

优雅停止：

```bash
run_dir=$(ls -dt /root/sub2api-soak-runs/run-* | head -n 1)
touch "$run_dir/control/STOP"
```

等待随机间隔时，脚本最长约 30 秒发现 STOP；等待人工确认时，runner 会写 `.reject`，后端不会访问 Anthropic。approval 模式下下游 `curl` 不设置总时长上限，因为人工检查可能超过 900 秒；批准后的上游连接/响应超时仍由 sub2api 自身的 HTTP 配置负责。

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

## 15. 输出与汇总

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
responses/*.raw
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

## 16. 通过标准

- Session F 的主请求始终严格串行，从不并发修改历史；
- Session F 完成 30 轮，或明确因为人工拒绝、5h/7d/deadline/错误保护停止；
- Session F 的下游 metadata session ID 跨 30 轮稳定；
- Session F 的上游 session ID 跨 30 轮稳定；
- 首轮一个 preview 同时包含 quota、title、main；成功的 quota/title 不会无故重复；
- 后续每个 preview 只包含一个 main，自动重试也必须形成新的 main-only stage；
- 每个最终上游 stage 都先完整展示，确认前没有 Anthropic transport 调用、manifest 行或 state 推进；
- 首轮三条 `send start` 严格按 quota → title → main 出现，quota → title 的间隔等于终端和 `manifest.tsv` 记录的随机延迟；
- quota、title、main 的最终 session ID 完全相同，title 内容没有进入 main 历史；
- 计划 hit 大多数出现长前缀量级 cache read；
- 计划 TTL miss/cold 大多数出现 cache creation；
- 没有无法解释的 400、401、403、429 或 5xx；
- sub2api、Redis、PostgreSQL 持续健康；
- 5h/7d 没有越过设定的安全目标；
- 每个请求都能由 schedule、manifest、请求快照和响应解释。

提示词回答质量不是本轮主要通过标准；重点是请求链路、身份、缓存、调度和保护行为。
