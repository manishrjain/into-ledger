# into-ledger Agent Guidelines

## Build & Test Commands
- **Build**: `go build .`
- **Format check**: `gofmt -l .`
- **Format write**: `gofmt -w .`
- **Vet/Lint**: `go vet ./...`
- **Test all**: `go test -v ./...`
- **Test single**: `go test -v -run TestName`

## Code Style
- **Language**: Go 1.25.1
- **Imports**: Group stdlib, blank line, then external packages (yaml, bolt, color, bayesian, keys, errors)
- **Error handling**: Use `github.com/pkg/errors` for wrapping errors with context
- **Naming**: CamelCase for exports, camelCase for unexported, acronyms uppercase (CSV, ID)
- **Constants**: Use const blocks with iota for enums
- **Regexp**: Compile patterns as package-level vars with `r` prefix (e.g., `rtxn`, `rto`)
- **Time format**: Use Go's reference time "2006/01/02" for date parsing
- **Struct tags**: Use yaml tags for config serialization
- **File structure**: Single package `main`, separate concerns (csv.go, plaid.go, main.go)
- **No unnecessary comments**: Code should be self-documenting