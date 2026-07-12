<div align="center">

# grokcli2api-go

**Expose the Grok CLI upstream as OpenAI- and Anthropic-compatible APIs**

A lightweight, deployable Go compatibility layer with streaming and multi-account scheduling

[![CI](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-663399.svg)](LICENSE)
[![Container](https://img.shields.io/badge/GHCR-grokcli2api--go-2496ED?logo=docker&logoColor=white)](https://github.com/Futureppo/grokcli2api-go/pkgs/container/grokcli2api-go)

[Quick Start](#quick-start) · [API Compatibility](#api-compatibility) · [Configuration](#configuration) · [Endpoints](#endpoints) · [Contributing](#development-and-contributing)

[简体中文](README.md) · **English**

</div>

---

`grokcli2api-go` is an unofficial API compatibility service written in Go. It translates the upstream API used by Grok CLI into OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages formats, allowing existing applications to connect by changing their API Base URL in most cases.

The project uses only the Go standard library at runtime and provides a multi-account OAuth credential pool, automatic token refresh, model discovery, session affinity, retries, and capacity backpressure. It is suitable for local development, internal services, and containerized deployments.

> [!IMPORTANT]
> This project is an unofficial compatibility layer and is not affiliated with or endorsed by xAI, X, OpenAI, or Anthropic. Users are responsible for complying with applicable terms of service and for any compatibility, availability, or account risks associated with using a non-public upstream API.

## Core Capabilities

| Category | Capabilities |
| --- | --- |
| API compatibility | OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, and native Grok CLI Responses passthrough |
| Response modes | Streaming SSE and non-streaming responses for common SDKs and HTTP clients |
| Credential management | Multi-account OAuth pool, automatic refresh, directory hot reload, and atomic persistence |
| Smart scheduling | Account rotation, session affinity, capability-aware routing, retries, and quota cooldowns |
| Concurrency control | Per-account concurrency limits and capacity backpressure to reduce 429 retry storms |
| Model discovery | Per-account upstream catalogs with caching, aggregation, and deduplication |
| Access protection | One or more local API keys through Bearer, `x-api-key`, or `api-key` headers |
| Network support | HTTP, HTTPS, SOCKS5, and SOCKS5H outbound proxies with standard `NO_PROXY` rules |
| Deployment | Single binary, graceful shutdown, multi-stage Docker build, and Docker Compose configuration |

## How It Works

```mermaid
flowchart LR
    A[OpenAI / Anthropic clients] --> B[Optional local API-key authentication]
    B --> C[Protocol conversion and streaming adaptation]
    C --> D[Model-aware account scheduler]
    D --> E1[OAuth account A]
    D --> E2[OAuth account B]
    D --> E3[OAuth account N]
    E1 --> F[Grok CLI upstream]
    E2 --> F
    E3 --> F
```

The service is optimized for different subscription tiers, and each request is routed only to a valid account that advertises support for the requested model.

## API Compatibility

| Protocol | Endpoint | Streaming | Non-streaming |
| --- | --- | :---: | :---: |
| OpenAI | `POST /v1/chat/completions` | ✓ | ✓ |
| OpenAI | `POST /v1/responses` | ✓ | ✓ |
| Anthropic | `POST /v1/messages` | ✓ | ✓ |
| OpenAI | `GET /v1/models` | — | ✓ |

The compatibility layer preserves commonly used request fields, response structures, and streaming events where possible, but it does not guarantee support for every parameter or behavior of the official APIs. When integrating through an API aggregation project such as New API, enable **passthrough** for all request parameters.

## Quick Start

### 1. Prepare the Project

You will need:

- Docker and Docker Compose, or Go 1.23 or later;
- at least one valid Grok CLI OAuth JSON credential; and
- a writable credential directory.

```bash
git clone https://github.com/Futureppo/grokcli2api-go.git
cd grokcli2api-go
cp .env.example .env
mkdir auths
```

Windows PowerShell:

```powershell
git clone https://github.com/Futureppo/grokcli2api-go.git
Set-Location grokcli2api-go
Copy-Item .env.example .env
New-Item -ItemType Directory -Force auths
```

Place each account's OAuth JSON file directly under `auths/`, with one account per file:

```text
auths/
├── account-1.json
├── account-2.json
└── account-n.json
```

Credentials typically need a usable access token, refresh metadata, and a stable account identity. The service scans only the first level of this directory and does not search subdirectories recursively.

> [!CAUTION]
> `auths/` is ignored by Git, but it must still be treated as a sensitive directory. The service hot-reloads credentials and writes refreshed tokens and model catalogs back to their original files, so the directory and files must be writable.

### 2. Configure a Local Access Key

Edit `.env` and replace the example value with a strong random key used only by your clients:

```dotenv
GROK_API_KEYS=sk-kfcvivo50
```

Local API keys protect this service and are separate from upstream OAuth credentials. Leaving the value empty disables access protection, which is not recommended in any environment reachable by other devices.

### 3. Start the Service

#### Docker Compose (recommended)

```bash
docker compose up -d
docker compose ps
```

View logs or stop the service:

```bash
docker compose logs -f
docker compose down
```

#### Run from source

```bash
go run ./cmd/grok2api
```

#### Use the prebuilt image

```bash
docker pull ghcr.io/futureppo/grokcli2api-go:latest
docker run --rm -p 8088:8088 --env-file .env \
  -v "$(pwd)/auths:/auths" \
  -e GROK_AUTHS_DIR=/auths \
  ghcr.io/futureppo/grokcli2api-go:latest
```

Docker Compose pulls and runs `ghcr.io/futureppo/grokcli2api-go:latest` by default. Every push publishes a `sha-<commit>` tag and a matching branch tag; pushes to `main` also update `latest`.

### 4. Verify the Service

The service listens on `http://0.0.0.0:8088` by default. Replace the key below with the value configured in `.env`:

```bash
curl http://localhost:8088/

curl http://localhost:8088/v1/models \
  -H "Authorization: Bearer sk-kfcvivo50"
```

## Usage Examples

The examples below use `sk-kfcvivo50` as a placeholder. Replace it with your local API key.

### OpenAI Chat Completions

```bash
curl http://localhost:8088/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-kfcvivo50" \
  -d '{
    "model": "grok-4.5",
    "messages": [
      {"role": "user", "content": "Hello!"}
    ]
  }'
```

### OpenAI Responses

```bash
curl http://localhost:8088/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-kfcvivo50" \
  -d '{
    "model": "grok-4.5",
    "input": "Explain what an API compatibility layer does."
  }'
```

### Anthropic Messages

```bash
curl http://localhost:8088/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-kfcvivo50" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "grok-4.5",
    "max_tokens": 512,
    "messages": [
      {"role": "user", "content": "Hello!"}
    ]
  }'
```

If neither `GROK_API_KEYS` nor `GROK_API_KEY` is configured, remove the local API-key header from these examples.

## Session Affinity and Account Scheduling

When multiple clients share one local API key, send a stable, non-sensitive identifier for each conversation:

```http
X-Grok-Session-ID: conversation-123
```

The service also recognizes the following fields as affinity identifiers, in order:

- OpenAI `prompt_cache_key`
- OpenAI `previous_response_id`
- OpenAI `user`
- Anthropic `metadata.user_id`

Local API keys and client IP addresses are never used for account affinity. Affinity mappings are stored only in memory and are bounded by a TTL and maximum capacity.

## Configuration

The service loads environment variables that are not already set from a `.env` file in the current working directory. See [`.env.example`](.env.example) for the complete template and advanced client-identity options.

### Server

| Environment variable | Default when unset | Description |
| --- | --- | --- |
| `GROK2API_HOST` | `0.0.0.0` | Service bind address |
| `GROK2API_PORT` | `8088` | Service bind port |
| `GROK2API_LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, or `ERROR` |
| `GROK_API_KEYS` | empty | Comma-separated local access keys; separate keys may be assigned to different clients |
| `GROK_API_KEY` | empty | Backward-compatible alias for one local access key |

When local access protection is enabled, protected endpoints accept any of these headers:

- `Authorization: Bearer <key>`
- `x-api-key: <key>`
- `api-key: <key>`

### Credential Pool and Scheduling

| Environment variable | Default when unset | Description |
| --- | --- | --- |
| `GROK_AUTHS_DIR` | `./auths` | Writable, non-recursive OAuth JSON directory |
| `GROK_AUTHS_RELOAD_INTERVAL` | `30s` | Credential directory hot-reload interval |
| `GROK_AUTH_REFRESH_CONCURRENCY` | `4` | Maximum concurrent OAuth refreshes |
| `GROK_ACCOUNT_MAX_INFLIGHT` | `16` | Maximum upstream requests in flight per account; excess requests wait for capacity |
| `GROK_MODELS_REFRESH_INTERVAL` | `6h` | Per-account model-catalog refresh interval |
| `GROK_RETRY_MAX_ATTEMPTS` | `3` | Maximum number of distinct accounts tried per request |
| `GROK_RETRY_BASE_DELAY` | `200ms` | Base delay for retryable network and upstream 5xx failures |
| `GROK_RATE_LIMIT_COOLDOWN` | `1m` | Cooldown when an upstream 429 omits `Retry-After` |
| `GROK_QUOTA_COOLDOWN` | `24h` | Default cooldown after quota exhaustion |
| `GROK_AFFINITY_TTL` | `1h` | Lifetime of in-memory session-affinity mappings |
| `GROK_AFFINITY_MAX_ENTRIES` | `100000` | Maximum number of affinity-cache entries |

Free-model quota cooldowns are isolated by account and model. An exhausted spending limit cools down the entire account.

### Upstream and Network

| Environment variable | Default when unset | Description |
| --- | --- | --- |
| `GROK_CHAT_PROXY_BASE_URL` | `https://cli-chat-proxy.grok.com` | Grok CLI upstream URL |
| `GROK_CHAT_PROXY_VERSION` | `v1` | Upstream API version |
| `GROK_STREAM_COMPRESSION` | `identity` | `identity` avoids buffering SSE through gzip; `gzip` is a compatibility fallback |
| `GROK_PROXY_URL` | empty | HTTP(S), SOCKS5, or SOCKS5H outbound proxy |
| `GROK_NO_PROXY` | empty | Comma-separated proxy bypass rules |
| `GROK_TLS_INSECURE_SKIP_VERIFY` | `false` | Disable upstream TLS verification; controlled debugging only |

When `GROK_PROXY_URL` is unset, the service honors the standard `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `NO_PROXY` environment variables.

The `-host` and `-port` command-line flags override the corresponding environment variables. Use `-version` to print the current version:

```bash
go run ./cmd/grok2api -host 127.0.0.1 -port 8088
go run ./cmd/grok2api -version
```

## Endpoints

### Compatible APIs

| Method | Path | Authentication | Description |
| --- | --- | :---: | --- |
| `GET` | `/` | No | Service name, version, and project URL |
| `GET` | `/v1/models` | Optional | Deduplicated union of model catalogs from all valid accounts |
| `GET` | `/v1/models/{model_id}` | Optional | Details for a specific model |
| `GET` | `/v1/auth/api-key` | No | Local API-key protection status |
| `POST` | `/v1/chat/completions` | Optional | OpenAI-compatible Chat Completions |
| `POST` | `/v1/responses` | Optional | OpenAI-compatible Responses |
| `POST` | `/v1/messages` | Optional | Anthropic-compatible Messages |

“Optional” means authentication is required only when a local API key has been configured.

### Read-only Grok Passthrough APIs

| Method | Path |
| --- | --- |
| `GET` | `/v1/grok/settings` |
| `GET` | `/v1/grok/user` |
| `GET` | `/v1/grok/billing` |
| `GET` | `/v1/grok/mcp/configs` |
| `GET` | `/v1/grok/mcp/tools/list` |
| `GET` | `/v1/grok/feedback/config` |

At startup, the service reads cached model catalogs from credential JSON files and requests upstream `/v1/models` for accounts whose catalog is missing or older than the refresh interval. Newly hot-loaded accounts are discovered automatically. Normalized `models` and `models_updated_at` fields are persisted to the corresponding credential files and preserved during token refresh.

Actual model availability is always controlled by the upstream account. Query `/v1/models` before generating content and use the exact model ID returned by the service.

## Docker and Image Design

The project image uses a multi-stage build: the build stage runs the complete test suite and produces a CGO-free binary, while the runtime stage uses a non-root user and a minimal Alpine base image. The Compose configuration also enables a read-only root filesystem, drops Linux capabilities, sets `no-new-privileges`, and defines a health check by default.

Local `.env` and `auths/` data remain external configuration and credential storage. Rebuilding or recreating the container does not bake them into the image.

## Security

- Never commit or disclose OAuth tokens, API keys, authentication files, or unsanitized logs.
- Configure `GROK_API_KEYS` before exposing the service to a network, and enable HTTPS, access control, and rate limiting at the reverse proxy.
- Apply least-privilege file permissions to the credential directory and restrict which system users can access it.
- Do not enable `GROK_TLS_INSECURE_SKIP_VERIFY` outside a controlled debugging environment.
- Do not use session IDs, email addresses, or other sensitive data directly as affinity identifiers.
- Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/Futureppo/grokcli2api-go/security/advisories/new).

## Development and Contributing

### Local Checks

```bash
gofmt -w path/to/changed.go
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/grok2api
```

> `go test -race ./...` requires a platform supported by the Go Race Detector. The project CI runs this check on Linux.

### Live Load Testing

The project includes an opt-in live upstream load test that reports response headers, first event, first non-empty text, completion latency, and sample coverage:

```bash
GROK_LIVE_LOAD=1 GROK_LOAD_MODEL=grok-4 GROK_LOAD_STREAM=1 \
GROK_LOAD_WARMUP=4 GROK_LOAD_CONCURRENCY=4 GROK_LOAD_REQUESTS=16 \
GROK_LOAD_API=responses GROK_LOAD_AFFINITY=cache \
go test ./internal/server -run TestLiveGenerationLoad -v
```

- `GROK_LOAD_API`: `responses`, `chat`, or `anthropic`
- `GROK_LOAD_AFFINITY`: `none`, `session`, or `cache`
- `GROK_LOAD_INPUT_BYTES`: generate a test input of the specified size in bytes

Set `GROK2API_LOG_LEVEL=DEBUG` to inspect segmented timing logs that omit credentials, request bodies, and session identifiers.

Read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes. Report bugs and feature requests through [GitHub Issues](https://github.com/Futureppo/grokcli2api-go/issues). Pull requests should remain focused and include tests for protocol conversion, streaming events, or error handling as appropriate.

## License

This project is licensed under the [GNU Affero General Public License v3.0](LICENSE). When using, modifying, or distributing this project, comply with the corresponding license obligations.

---

<div align="center">

If this project helps you, contributions, issues, and stars are always welcome ⭐.

</div>
