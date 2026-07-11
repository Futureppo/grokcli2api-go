# Contributing

Thanks for contributing to `grokcli2api-go`.

## Development

Requirements: Go 1.23 or newer.

```bash
go test ./...
go vet ./...
go build ./cmd/grok2api
```

Run `gofmt` on changed Go files. Keep the production implementation on the Go standard library unless a dependency is clearly justified.

## Pull requests

- Keep changes focused and describe protocol compatibility implications.
- Add tests for request conversion, response conversion, streaming events, and errors as applicable.
- Never commit session tokens, API keys, authentication files, or unsanitized upstream traffic.
- Update the README when public endpoints, environment variables, or compatibility behavior changes.
