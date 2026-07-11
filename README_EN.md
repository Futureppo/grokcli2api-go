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
- Anthropic Messages API compatibility
- Streaming and non-streaming responses
- Grok CLI SessionToken and local authentication-file support
- Optional local API-key protection
- HTTP, HTTPS, SOCKS5, and SOCKS5H outbound proxies
- Built-in health checks, model discovery, OpenAPI documentation, and Swagger UI
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
- A valid Grok CLI SessionToken or an authentication file created by a signed-in Grok CLI installation

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

Edit `.env` and configure at least one upstream authentication method:

```dotenv
# Option 1: provide a SessionToken directly
GROK_SESSION_TOKEN=your-session-token

# Option 2: read the Grok CLI authentication file
# GROK_AUTH_FILE=~/.grok/auth.json
```

Start the service:

```bash
go run ./cmd/grok2api
```

The service listens on `http://0.0.0.0:8088` by default. Open `http://localhost:8088/docs` for interactive API documentation.

### Run with Docker

Pull the latest image directly from GitHub Container Registry:

```bash
docker pull ghcr.io/futureppo/grokcli2api-go:latest
docker run --rm -p 8088:8088 --env-file .env \
  ghcr.io/futureppo/grokcli2api-go:latest
```

Alternatively, build the image locally:

```bash
docker build -t grokcli2api-go .
docker run --rm -p 8088:8088 --env-file .env grokcli2api-go
```

When using an authentication file, mount it and use its path inside the container:

```bash
docker run --rm -p 8088:8088 \
  -v "$HOME/.grok:/home/app/.grok:ro" \
  -e GROK_AUTH_FILE=/home/app/.grok/auth.json \
  ghcr.io/futureppo/grokcli2api-go:latest
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

If neither `GROK_API_KEYS` nor `GROK_API_KEY` is configured, remove the local API-key header from these examples. Local API keys protect this service; they are separate from the SessionToken sent to the Grok upstream.

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

### Upstream authentication

| Environment variable | Default | Description |
| --- | --- | --- |
| `GROK_SESSION_TOKEN` | empty | Grok SessionToken; takes precedence when set |
| `GROK_AUTH_FILE` | empty | Path to a Grok CLI authentication JSON file; supports `~` |
| `GROK_OAUTH_CLIENT_ID` | empty | Reserved; the device OAuth flow is not implemented yet |

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
| `GET` | `/docs` | Swagger UI |
| `GET` | `/openapi.json` | OpenAPI 3.1 document |
| `GET` | `/v1/health` | Upstream health status |
| `GET` | `/v1/models` | List models |
| `GET` | `/v1/models/{model_id}` | Get model details |
| `GET` | `/v1/auth/api-key` | Local API-key protection status |
| `GET` | `/v1/auth/status` | Upstream authentication status |
| `POST` | `/v1/auth/refresh` | Reload or refresh upstream authentication |
| `POST` | `/v1/chat/completions` | OpenAI-compatible Chat Completions |
| `POST` | `/v1/responses` | OpenAI-compatible Responses |
| `POST` | `/v1/messages` | Anthropic-compatible Messages |

The service also provides a small set of read-only `/v1/grok/*` passthrough endpoints. See `/openapi.json` for the complete list in the running version.

Currently advertised model IDs include `grok-build`, `grok-4`, `grok-4.5`, `grok-auto`, `grok-4-fast-reasoning`, `grok-4-fast-non-reasoning`, `grok-3`, `grok-3-mini`, `grok-code-fast-1`, and `grok-2-vision`. Actual availability depends on the upstream service and account permissions.

## Security

- Never commit or disclose SessionTokens, API keys, authentication files, or unsanitized logs.
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
