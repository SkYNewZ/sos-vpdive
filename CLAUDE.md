# Repository Guidelines

Small support desk for a volunteer diving club. Prefer simple, robust solutions.

## Project Structure & Module Organization

`cmd/sos-vpdive/` contains CLI commands; `internal/` holds domain packages.
HTTP handlers, templates, JavaScript and CSS live in `internal/web/`.
`content.go` embeds `config/` YAML and `kb/` articles; migrations live in
`internal/store/migrations/`. Tests sit beside source; synthetic fixtures live
in `testdata/fixtures/`. Read `docs/development.md` for the affected subsystem's
invariants before editing; `README.md` covers setup and deployment.

## Build, Test, and Development Commands

- `make build`: compile CSS and the pure-Go binary into `bin/sos-vpdive`.
- `make run`: load an existing `.env`; serve `sos.localhost:8080` and
  `comite.localhost:8080`. Follow README setup; preserve existing local files.
- `make test`: run all Go tests with the race detector.
- `make lint`: run golangci-lint v2; `make fmt`: apply gofmt.
- `go run ./cmd/sos-vpdive validate-kb`: validate embedded articles.

## Coding Style & Naming Conventions

Use standard-library Go, gofmt tabs, goimports and `.golangci.yaml`.
Keep server-rendered HTML, vanilla JavaScript, Tailwind 4 and daisyUI 5;
Node is unnecessary for builds. Reuse domain helpers; justify new dependencies
in commits.

## Testing Guidelines

Use Go `testing` and existing testify helpers. Name files `*_test.go` and tests
`TestBehavior`; start with `go test ./internal/web -run TestName`.
Add regression tests for changed behavior; no numeric coverage gate exists.
Before delivery, inspect the diff and run relevant CI checks, including
`go vet ./...` and `./scripts/check-forbidden-files.sh`.
Browser-check UI changes; report blocked checks. See the development reference
for fixture regeneration, diagrams and isolated acceptance runs.

## Security & Agent Instructions

Never commit or copy `.local/` content, secrets, real exports or databases.
Encrypt personal data before storage; exclude it and tracking tokens from logs.
Preserve hostname isolation, Origin checks and bearer-protected imports.
Keep migrations append-only and model output committee-only.
Preserve `.env`, `admins/` and `data/`. Confirm configuration changes before applying.
