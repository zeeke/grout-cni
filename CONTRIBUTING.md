# Contributing to grout-cni

We welcome contributions! Here's how to get started.

## Development setup

1. Install [Go](https://go.dev/dl/) (see `go.mod` for the minimum version)
2. Install [golangci-lint](https://golangci-lint.run/welcome/install/)
3. Clone the repo and run `make test` to verify your setup

## Workflow

1. Fork the repo and create a feature branch from `main`
2. Make your changes
3. Run `make build && make test && make lint` — all three must pass
4. Open a pull request against `main`

## Code style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Use `slog` for structured logging
- Wrap errors with `fmt.Errorf("context: %w", err)`
- No globals except loggers; pass dependencies explicitly

## Testing

- Unit tests: `make test`
- Integration tests (requires Docker): `make e2e`
- Full Kubernetes e2e (requires kind): `make kind-e2e`

## Commit messages

Use short, imperative commit messages (e.g., "Add virtio CHECK support"). Reference issues where relevant.

## Reporting issues

Open an issue on GitHub. Include the grout-cni version, Go version, Kubernetes version, and relevant CNI config/logs.
