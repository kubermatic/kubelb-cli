# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
make build              # Build binary to bin/kubelb
make test               # Run all tests
go test ./internal/...  # Run specific package tests
go test -run TestName ./internal/logger/  # Run single test
make lint               # Run golangci-lint
make verify-all         # All checks (boilerplate, imports, licenses, shfmt)
make install            # Install to Go bin directory
```

## Architecture

### Edition Detection Flow

CLI auto-detects KubeLB backend edition (CE vs EE) at startup via `PersistentPreRunE`:

1. Check cache (`~/.kubelb/cache/`, 48h TTL)
2. If miss: query TenantState resource for edition field
3. Fallback: check if tunnel CRDs exist (EE-only)
4. Load appropriate API types from `k8c.io/kubelb/api/{ce,ee}/`

### Key Packages

- `cmd/` - Cobra commands, config init happens in root.go PersistentPreRunE
- `internal/edition/` - Edition detection logic
- `internal/config/` - Kubeconfig/tenant resolution with precedence: flags > env vars > error
- `internal/tunnel/connect.go` - HTTP/2 SSE client with auto-reconnect (exponential backoff, max 10 retries)
- `internal/logger/` - Custom slog-based logging with CLI/JSON/text formats

### Tenant Namespace

Tenant names auto-prefix with `tenant-` if not already prefixed. Namespace = `tenant-{name}`.

### Config Requirements

No defaults for kubeconfig or tenant - both must be explicitly provided via flags or env vars (KUBECONFIG, TENANT_NAME).

### Environment Variables

All flags can be configured via env vars. Precedence: flags > env vars > defaults.

**Global:**

| Env Var | Flag | Default |
|---------|------|---------|
| `KUBECONFIG` | `--kubeconfig` | (required) |
| `TENANT_NAME` | `--tenant` | (required) |
| `KUBELB_INSECURE_SKIP_VERIFY` | - | `false` |
| `KUBELB_LOG_LEVEL` | `--log-level` | - |
| `KUBELB_LOG_FORMAT` | `--log-format` | `cli` |
| `KUBELB_LOG_PATH` | `--log-file` | - |

**Ingress conversion (`kubelb ingress`):**

| Env Var | Flag | Default |
|---------|------|---------|
| `KUBELB_GATEWAY_NAME` | `--gateway-name` | `kubelb` |
| `KUBELB_GATEWAY_NAMESPACE` | `--gateway-namespace` | `kubelb` |
| `KUBELB_GATEWAY_CLASS` | `--gateway-class` | `kubelb` |
| `KUBELB_INGRESS_CLASS` | `--ingress-class` | (all) |
| `KUBELB_DOMAIN_REPLACE` | `--domain-replace` | - |
| `KUBELB_DOMAIN_SUFFIX` | `--domain-suffix` | - |
| `KUBELB_PROPAGATE_EXTERNAL_DNS` | `--propagate-external-dns` | `true` |
| `KUBELB_GATEWAY_ANNOTATIONS` | `--gateway-annotations` | - |
| `KUBELB_DISABLE_ENVOY_GATEWAY_FEATURES` | `--disable-envoy-gateway-features` | `false` |
| `KUBELB_COPY_TLS_SECRETS` | `--copy-tls-secrets` | `true` |

### Commands Skipping Config

version, help, completion, docs - skip kubeconfig/tenant loading.

## Module Path

`k8c.io/kubelb-cli`
