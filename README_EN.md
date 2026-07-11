# grokcli2api-go

[![CI](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Futureppo/grokcli2api-go/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go)](https://go.dev/)

[中文](README.md) | English

`grokcli2api-go` is a lightweight Go service with no third-party runtime dependencies. It translates the upstream API used by Grok CLI into OpenAI- and Anthropic-compatible APIs, allowing existing tools to connect by changing their API Base URL.

> [!IMPORTANT]
> This is an unofficial compatibility layer and is not affiliated with xAI, X, or OpenAI. You are responsible for complying with applicable terms of service and for any compatibility or account risks associated with using a non-public upstream API.

## Features

- OpenAI Chat Completions API compatibility
- OpenAI Responses API compatibility
- Grok CLI native Responses passthrough
- Anthropic Messages API compatibility
- Streaming and non-streaming responses
- Multi-account OAuth pool with automatic refresh and directory hot reload
- Session affinity, account rotation, retries, and quota cooldowns
- Per-account concurrency limits and capacity backpressure to reduce 429 retry storms
- Optional local API-key protection
- HTTP, HTTPS, SOCKS5, and SOCKS5H outbound proxies
- Per-account upstream model discovery, aggregation, and capability-aware scheduling
- Standard-library-only Go implementation for simple builds and deployments

## API compatibility

| Protocol | Endpoint | Streaming |
| --- | --- | :---: |
| OpenAI | `POST /v1/chat/completions` | ✓ |
| OpenAI | `POST /v1/responses` | ✓ |
| Anthropic | `POST /v1/messages` | ✓ |
| OpenAI | `GET /v1/models` | — |

The compatibility layer preserves commonly used request and response formats where possible, but it does not guarantee support for every parameter or behavior of the official APIs.

## Quick start

### Requirements

- Go 1.23 or later
- At least one Grok OAuth JSON credential containing an `access_token` and `refresh_token`

### Run from source

```bash
git clone https://github.com/Futureppo/grokcli2api-go.git
cd grokcli2api-go
cp .env.example .env
```

On Windows PowerShell, use:

```powershell
Copy-Item .env.example .env
```

Create the credential directory and put one OAuth JSON file per account directly inside it:

```bash
mkdir auths
# auths/account-1.json
# auths/account-2.json
```

`auths` is ignored by Git. The service hot-reloads files and atomically writes refreshed tokens back, so the directory must be writable. It also queries the upstream model catalog for every account and writes normalized `models` and `models_updated_at` fields back to the corresponding credential JSON.

Start the service:

```bash
go run ./cmd/grok2api
```

The service listens on `http://0.0.0.0:8088` by default.

### Run with Docker

Pull the latest image directly from GitHub Container Registry:

```bash
docker pull ghcr.io/futureppo/grokcli2api-go:latest
docker run --rm -p 8088:8088 --env-file .env \
  -v "$(pwd)/auths:/auths" \
  -e GROK_AUTHS_DIR=/auths \
  ghcr.io/futureppo/grokcli2api-go:latest
```

Alternatively, build the image locally:

```bash
docker build -t grokcli2api-go .
docker run --rm -p 8088:8088 --env-file .env \
  -v "$(pwd)/auths:/auths" -e GROK_AUTHS_DIR=/auths grokcli2api-go
```

Every push publishes a `sha-<commit>` tag and a matching branch tag. Pushes to `main` also update `latest`.

## Usage examples

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

If neither `GROK_API_KEYS` nor `GROK_API_KEY` is configured, remove the local API-key header from these examples. Local API keys protect this service; they are separate from upstream OAuth credentials.

When many users share one local API key, send a stable `X-Grok-Session-ID` per conversation. The service also recognizes `prompt_cache_key`, `previous_response_id`, `user`, and Anthropic `metadata.user_id`; API keys and client IP addresses are never used for affinity.

## Configuration

The service loads environment variables that are not already set from a `.env` file in the current working directory. See [`.env.example`](.env.example) for the complete template.

### Server

| Environment variable | Default | Description |
| --- | --- | --- |
| `GROK2API_HOST` | `0.0.0.0` | Bind address |
| `GROK2API_PORT` | `8088` | Bind port |
| `GROK2API_LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, or `ERROR` |
| `GROK_API_KEYS` | empty | Comma-separated local access keys |
| `GROK_API_KEY` | empty | Backward-compatible alias for one local key |

### Credential pool and scheduling

| Environment variable | Default | Description |
| --- | --- | --- |
| `GROK_AUTHS_DIR` | `./auths` | Writable, non-recursive OAuth JSON directory |
| `GROK_AUTHS_RELOAD_INTERVAL` | `30s` | Credential hot-reload interval |
| `GROK_AUTH_REFRESH_CONCURRENCY` | `4` | Maximum concurrent OAuth refreshes |
| `GROK_ACCOUNT_MAX_INFLIGHT` | `16` | Maximum upstream requests in flight per account; excess requests wait for capacity |
| `GROK_MODELS_REFRESH_INTERVAL` | `6h` | Per-account model-catalog refresh interval |
| `GROK_RETRY_MAX_ATTEMPTS` | `3` | Maximum distinct accounts tried per request |
| `GROK_RETRY_BASE_DELAY` | `200ms` | Base delay for retryable network and 5xx failures |
| `GROK_RATE_LIMIT_COOLDOWN` | `1m` | 429 cooldown when `Retry-After` is absent |
| `GROK_QUOTA_COOLDOWN` | `24h` | Cooldown for quota-exhausted accounts |
| `GROK_AFFINITY_TTL` | `1h` | In-memory session-affinity lifetime |
| `GROK_AFFINITY_MAX_ENTRIES` | `100000` | Maximum affinity-cache entries |

When local API-key protection is enabled, protected endpoints accept any of these headers:

- `Authorization: Bearer <key>`
- `x-api-key: <key>`
- `api-key: <key>`

### Upstream and network

| Environment variable | Default | Description |
| --- | --- | --- |
| `GROK_CHAT_PROXY_BASE_URL` | `https://cli-chat-proxy.grok.com` | Grok CLI upstream URL |
| `GROK_CHAT_PROXY_VERSION` | `v1` | Upstream API version |
| `GROK_PROXY_URL` | empty | HTTP(S), SOCKS5, or SOCKS5H outbound proxy |
| `GROK_NO_PROXY` | empty | Comma-separated proxy bypass rules |
| `GROK_TLS_INSECURE_SKIP_VERIFY` | `false` | Disable upstream TLS verification; controlled debugging only |

When `GROK_PROXY_URL` is unset, the service honors the standard `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `NO_PROXY` environment variables. Advanced client-identity options are also documented in [`.env.example`](.env.example).

The `-host` and `-port` command-line flags override the corresponding environment variables. Use `-version` to print the current version:

```bash
go run ./cmd/grok2api -host 127.0.0.1 -port 8088
go run ./cmd/grok2api -version
```

## Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/` | Service information |
| `GET` | `/v1/models` | List models (authenticated when a local API key is configured) |
| `GET` | `/v1/models/{model_id}` | Get model details (authenticated when a local API key is configured) |
| `GET` | `/v1/auth/api-key` | Local API-key protection status |
| `POST` | `/v1/chat/completions` | OpenAI-compatible Chat Completions |
| `POST` | `/v1/responses` | OpenAI-compatible Responses |
| `POST` | `/v1/messages` | Anthropic-compatible Messages |

The service also provides read-only `/v1/grok/settings`, `user`, `billing`, `mcp/configs`, `mcp/tools/list`, and `feedback/config` passthrough endpoints.

The model list is not hardcoded locally. At startup, the service reads cached catalogs from credential JSON files and calls upstream `/v1/models` for accounts whose catalog is missing or older than the refresh interval. Newly hot-loaded accounts are discovered automatically. `GET /v1/models` returns the deduplicated union across valid accounts, and requests are scheduled only to accounts that advertise the requested model. The service does not add model aliases or rewrite requested model IDs.

The persisted `models` and `models_updated_at` fields are used only for capability discovery and scheduling and are preserved during token refresh. Actual availability remains controlled by the upstream account. Query the catalog before sending generation requests:

```bash
curl http://localhost:8088/v1/models \
  -H "Authorization: Bearer local-api-key"
```

## Security

- Never commit or disclose OAuth tokens, API keys, authentication files, or unsanitized logs.
- Configure `GROK_API_KEYS` before exposing the service to a network. Use HTTPS, access controls, and rate limiting at the reverse proxy.
- Do not enable `GROK_TLS_INSECURE_SKIP_VERIFY` outside a controlled debugging environment.
- Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/Futureppo/grokcli2api-go/security/advisories/new).

## Development and contributing

```bash
go test ./...
go vet ./...
go build ./cmd/grok2api
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes. Report bugs and feature requests through [GitHub Issues](https://github.com/Futureppo/grokcli2api-go/issues).
