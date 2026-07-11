# grokcli2api-go

[![CI](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go)](https://go.dev/)

中文 | [English](README_EN.md)

`grokcli2api-go` 是一个超轻量、无第三方运行时依赖的 Go 服务，将 Grok CLI 使用的上游接口转换为 OpenAI 与 Anthropic 兼容 API。已有工具只需修改 API Base URL，即可通过常见的 SDK 或 HTTP 客户端接入。

> [!IMPORTANT]
> 本项目是非官方兼容层，与 xAI、X 或 OpenAI 没有关联。请遵守相关服务条款，并自行承担使用非公开上游接口可能带来的兼容性与账号风险。

## 功能特性

- 支持 OpenAI Chat Completions API
- 支持 OpenAI Responses API
- 支持 Grok CLI 原生 Responses 透传
- 支持 Anthropic Messages API
- 支持流式与非流式响应
- 支持多账号 OAuth 凭证池、自动刷新和目录热加载
- 支持会话亲和、账号轮询、自动重试和额度冷却
- 支持账号级并发上限和容量背压，减少高并发 429 重试
- 可配置本地 API Key 访问保护
- 支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H 出站代理
- 按账号发现并聚合上游模型，按模型能力调度请求
- 仅使用 Go 标准库，便于构建和部署

## API 兼容性

| 协议 | 接口 | 流式响应 |
| --- | --- | :---: |
| OpenAI | `POST /v1/chat/completions` | ✓ |
| OpenAI | `POST /v1/responses` | ✓ |
| Anthropic | `POST /v1/messages` | ✓ |
| OpenAI | `GET /v1/models` | — |

兼容层会尽量保留常用请求和响应格式，但不能保证覆盖官方 API 的全部参数和行为。

## 快速开始

### 前置条件

- Go 1.23 或更高版本
- 至少一个包含 OAuth `access_token` 与 `refresh_token` 的 Grok 凭证 JSON

### 从源码运行

```bash
git clone https://github.com/Futureppo/grokcli2api-go.git
cd grokcli2api-go
cp .env.example .env
```

Windows PowerShell 可使用：

```powershell
Copy-Item .env.example .env
```

创建凭证目录，将每个账号的 OAuth JSON 直接放入其中：

```bash
mkdir auths
# auths/account-1.json
# auths/account-2.json
```

`auths` 已被 Git 忽略。服务会热加载文件并原子写回刷新的 token，因此目录必须可写。服务还会查询每个账号的上游模型目录，并将规范化后的 `models` 和 `models_updated_at` 字段写回对应凭证 JSON。

启动服务：

```bash
go run ./cmd/grok2api
```

服务默认监听 `http://0.0.0.0:8088`。

### 使用 Docker

直接从 GitHub Container Registry 拉取最新镜像：

```bash
docker pull ghcr.io/futureppo/grokcli2api-go:latest
docker run --rm -p 8088:8088 --env-file .env \
  -v "$(pwd)/auths:/auths" \
  -e GROK_AUTHS_DIR=/auths \
  ghcr.io/futureppo/grokcli2api-go:latest
```

也可以在本地构建：

```bash
docker build -t grokcli2api-go .
docker run --rm -p 8088:8088 --env-file .env \
  -v "$(pwd)/auths:/auths" -e GROK_AUTHS_DIR=/auths grokcli2api-go
```

每次推送都会发布 `sha-<commit>` 和对应的分支标签；`main` 分支还会更新 `latest` 标签。

## 调用示例

### OpenAI Chat Completions

```bash
curl http://localhost:8088/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer local-api-key" \
  -d '{
    "model": "grok-4",
    "messages": [
      {"role": "user", "content": "Hello!"}
    ]
  }'
```

### OpenAI Responses

```bash
curl http://localhost:8088/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer local-api-key" \
  -d '{
    "model": "grok-4",
    "input": "Explain what an API compatibility layer does."
  }'
```

### Anthropic Messages

```bash
curl http://localhost:8088/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: local-api-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "grok-4",
    "max_tokens": 512,
    "messages": [
      {"role": "user", "content": "Hello!"}
    ]
  }'
```

如果没有设置 `GROK_API_KEYS` 或 `GROK_API_KEY`，请从示例中移除本地 API Key 请求头。它们只用于保护本服务，不是上游 OAuth 凭证。

并发共享同一个本地 API Key 时，可通过 `X-Grok-Session-ID` 为每个会话提供稳定标识。服务也会自动识别 `prompt_cache_key`、`previous_response_id`、`user` 和 Anthropic `metadata.user_id`，不会使用 API Key 或客户端 IP 做账号亲和。

## 配置

程序会从当前工作目录的 `.env` 文件加载尚未设置的环境变量。完整模板见 [`.env.example`](.env.example)。

### 服务配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GROK2API_HOST` | `0.0.0.0` | 监听地址 |
| `GROK2API_PORT` | `8088` | 监听端口 |
| `GROK2API_LOG_LEVEL` | `INFO` | 日志等级：`DEBUG`、`INFO`、`WARN` 或 `ERROR` |
| `GROK_API_KEYS` | 空 | 逗号分隔的本地访问密钥 |
| `GROK_API_KEY` | 空 | 单个本地访问密钥的兼容别名 |

### 凭证池与调度

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GROK_AUTHS_DIR` | `./auths` | 非递归扫描的可写 OAuth JSON 目录 |
| `GROK_AUTHS_RELOAD_INTERVAL` | `30s` | 凭证目录热加载周期 |
| `GROK_AUTH_REFRESH_CONCURRENCY` | `4` | OAuth 刷新并发数 |
| `GROK_ACCOUNT_MAX_INFLIGHT` | `16` | 每账号最大上游在途请求数；超出后等待可用容量 |
| `GROK_MODELS_REFRESH_INTERVAL` | `6h` | 每个账号模型目录的刷新周期 |
| `GROK_RETRY_MAX_ATTEMPTS` | `3` | 单个请求最多尝试的不同账号数 |
| `GROK_RETRY_BASE_DELAY` | `200ms` | 可重试网络与 5xx 错误的基础退避 |
| `GROK_RATE_LIMIT_COOLDOWN` | `1m` | 无 `Retry-After` 时的 429 冷却时间 |
| `GROK_QUOTA_COOLDOWN` | `24h` | 额度耗尽账号的冷却时间 |
| `GROK_AFFINITY_TTL` | `1h` | 内存会话亲和有效期 |
| `GROK_AFFINITY_MAX_ENTRIES` | `100000` | 会话亲和缓存容量上限 |

设置本地 API Key 后，受保护接口接受以下任一种请求头：

- `Authorization: Bearer <key>`
- `x-api-key: <key>`
- `api-key: <key>`

### 上游与网络

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GROK_CHAT_PROXY_BASE_URL` | `https://cli-chat-proxy.grok.com` | Grok CLI 上游地址 |
| `GROK_CHAT_PROXY_VERSION` | `v1` | 上游 API 版本 |
| `GROK_PROXY_URL` | 空 | 出站代理，支持 HTTP(S)、SOCKS5、SOCKS5H |
| `GROK_NO_PROXY` | 空 | 逗号分隔的代理绕过规则 |
| `GROK_TLS_INSECURE_SKIP_VERIFY` | `false` | 跳过上游 TLS 验证，仅用于受控调试环境 |

未设置 `GROK_PROXY_URL` 时，程序遵循标准的 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY` 和 `NO_PROXY` 环境变量。客户端标识相关的高级选项也记录在 [`.env.example`](.env.example) 中。

命令行参数 `-host` 和 `-port` 可覆盖对应环境变量，使用 `-version` 可输出当前版本：

```bash
go run ./cmd/grok2api -host 127.0.0.1 -port 8088
go run ./cmd/grok2api -version
```

## 可用接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/` | 服务信息 |
| `GET` | `/v1/models` | 模型列表（配置本地 API Key 时需鉴权） |
| `GET` | `/v1/models/{model_id}` | 模型详情（配置本地 API Key 时需鉴权） |
| `GET` | `/v1/auth/api-key` | 本地 API Key 保护状态 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions 兼容接口 |
| `POST` | `/v1/responses` | OpenAI Responses 兼容接口 |
| `POST` | `/v1/messages` | Anthropic Messages 兼容接口 |

服务还提供 `/v1/grok/settings`、`user`、`billing`、`mcp/configs`、`mcp/tools/list` 和 `feedback/config` 只读透传接口。

模型列表不是本地硬编码的。启动时服务会读取凭证 JSON 中的缓存目录，并为缺少或超过刷新周期的账号调用上游 `/v1/models`；新增账号也会在热加载后自动发现。`GET /v1/models` 返回所有有效账号目录的去重并集，请求只会调度到声明支持目标模型的账号。服务不会添加模型别名或改写请求中的模型 ID。

每个凭证文件中持久化的 `models` 与 `models_updated_at` 仅用于能力目录和调度，刷新 token 时会保留；实际支持范围仍以上游账号返回结果为准。调用生成接口前可先查询：

```bash
curl http://localhost:8088/v1/models \
  -H "Authorization: Bearer local-api-key"
```

## 安全建议

- 不要提交或公开 OAuth token、API Key、认证文件及未脱敏日志。
- 对外网提供服务前务必设置 `GROK_API_KEYS`，并在反向代理层启用 HTTPS、访问控制和限流。
- 除非处于受控调试环境，否则不要启用 `GROK_TLS_INSECURE_SKIP_VERIFY`。
- 安全漏洞请通过 [GitHub Security Advisories](https://github.com/Futureppo/grokcli2api-go/security/advisories/new) 私下报告。

## 开发与贡献

```bash
go test ./...
go vet ./...
go build ./cmd/grok2api
```

提交代码前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。Bug 和功能建议可通过 [GitHub Issues](https://github.com/Futureppo/grokcli2api-go/issues) 提交。
