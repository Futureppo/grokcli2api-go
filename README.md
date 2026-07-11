# grokcli2api-go

[![CI](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go)](https://go.dev/)

中文 | [English](README_EN.md)

`grokcli2api-go` 是一个轻量、无第三方运行时依赖的 Go 服务，将 Grok CLI 使用的上游接口转换为 OpenAI 与 Anthropic 兼容 API。已有工具只需修改 API Base URL，即可通过常见的 SDK 或 HTTP 客户端接入。

> [!IMPORTANT]
> 本项目是非官方兼容层，与 xAI、X 或 OpenAI 没有关联。请遵守相关服务条款，并自行承担使用非公开上游接口可能带来的兼容性与账号风险。

## 功能特性

- 支持 OpenAI Chat Completions API
- 支持 OpenAI Responses API
- 支持 Anthropic Messages API
- 支持流式与非流式响应
- 支持 Grok CLI SessionToken 和本地认证文件
- 可配置本地 API Key 访问保护
- 支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H 出站代理
- 内置健康检查、模型列表、OpenAPI 文档和 Swagger UI
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
- 一个有效的 Grok CLI SessionToken，或已登录 Grok CLI 生成的认证文件

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

编辑 `.env`，至少配置一种上游认证方式：

```dotenv
# 方式一：直接使用 SessionToken
GROK_SESSION_TOKEN=your-session-token

# 方式二：读取 Grok CLI 的认证文件
# GROK_AUTH_FILE=~/.grok/auth.json
```

启动服务：

```bash
go run ./cmd/grok2api
```

服务默认监听 `http://0.0.0.0:8088`。打开 `http://localhost:8088/docs` 可查看交互式 API 文档。

### 使用 Docker

直接从 GitHub Container Registry 拉取最新镜像：

```bash
docker pull ghcr.io/futureppo/grokcli2api-go:latest
docker run --rm -p 8088:8088 --env-file .env \
  ghcr.io/futureppo/grokcli2api-go:latest
```

也可以在本地构建：

```bash
docker build -t grokcli2api-go .
docker run --rm -p 8088:8088 --env-file .env grokcli2api-go
```

如果使用认证文件，需要将文件挂载到容器并使用容器内路径：

```bash
docker run --rm -p 8088:8088 \
  -v "$HOME/.grok:/home/app/.grok:ro" \
  -e GROK_AUTH_FILE=/home/app/.grok/auth.json \
  ghcr.io/futureppo/grokcli2api-go:latest
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

如果没有设置 `GROK_API_KEYS` 或 `GROK_API_KEY`，请从示例中移除本地 API Key 请求头。它们只用于保护本服务，不是发送给 Grok 上游的 SessionToken。

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

### 上游认证

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GROK_SESSION_TOKEN` | 空 | 直接提供 Grok SessionToken，优先级最高 |
| `GROK_AUTH_FILE` | 空 | Grok CLI 认证 JSON 文件路径，支持 `~` |
| `GROK_OAUTH_CLIENT_ID` | 空 | 预留配置；设备 OAuth 流程目前尚未实现 |

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
| `GET` | `/docs` | Swagger UI |
| `GET` | `/openapi.json` | OpenAPI 3.1 文档 |
| `GET` | `/v1/health` | 上游健康状态 |
| `GET` | `/v1/models` | 模型列表 |
| `GET` | `/v1/models/{model_id}` | 模型详情 |
| `GET` | `/v1/auth/api-key` | 本地 API Key 保护状态 |
| `GET` | `/v1/auth/status` | 上游认证状态 |
| `POST` | `/v1/auth/refresh` | 重新读取或刷新上游认证 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions 兼容接口 |
| `POST` | `/v1/responses` | OpenAI Responses 兼容接口 |
| `POST` | `/v1/messages` | Anthropic Messages 兼容接口 |

服务还提供少量 `/v1/grok/*` 只读透传接口。请通过 `/openapi.json` 查看当前版本的完整列表。

当前公开的模型标识包括 `grok-build`、`grok-4`、`grok-4.5`、`grok-auto`、`grok-4-fast-reasoning`、`grok-4-fast-non-reasoning`、`grok-3`、`grok-3-mini`、`grok-code-fast-1` 和 `grok-2-vision`。实际可用性取决于上游服务和账号权限。

## 安全建议

- 不要提交或公开 SessionToken、API Key、认证文件及未脱敏日志。
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
