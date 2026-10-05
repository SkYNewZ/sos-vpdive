# sos-vpdive

A small support desk for a volunteer-run diving club whose members use the
VPDive platform. Members file requests through a public form; a few committee
members handle them. One Go binary serves two host names: the members site
and the committee site. Simplicity and robustness beat features.

Status: lot 1 (foundation and members whitelist). The request form, mails and
the requests board come next.

## Run it locally

Requirements: Go (see `go.mod`), `make`, `curl`.

```sh
cp .env.example .env
# In .env: APP_ENV=development, BASE_URL=http://sos.localhost:8080,
# ADMIN_BASE_URL=http://comite.localhost:8080, SECRET_KEY=$(openssl rand -base64 32),
# and empty TURNSTILE_* and S3_* values.
go run ./cmd/sos-vpdive hash-password        # type a password twice
cp admins.example.yaml admins.yaml           # paste the hash, adjust names
make run
```

Open http://comite.localhost:8080 (browsers resolve `*.localhost` to your
machine and accept the `__Host-` session cookie there) and import the members
list on the Imports page. http://sos.localhost:8080 is the members site.

`make test` runs the tests, `make lint` the linter, `make css` the stylesheet.

## Deploy with Docker Compose

```sh
docker build -t sos-vpdive:local .
cp .env.example .env                                    # fill every required value
docker run --rm -it sos-vpdive:local hash-password      # once per account
cp admins.example.yaml admins.yaml                      # paste the hashes
docker compose up -d --wait
```

The image is distroless (`gcr.io/distroless/static-debian13:nonroot`): no
shell, user 65532, read-only root file system. The database lives in the
`/data` volume; the accounts file is mounted read-only.

### Behind a reverse proxy

- Terminate TLS at the proxy and forward both host names to port 8080 with the
  `Host` header unchanged.
- Set `X-Forwarded-For` and list the proxy's addresses in `TRUSTED_PROXIES`;
  otherwise the visitor address is the proxy's.
- Do not log request paths of the members site: tracking links carry a secret.
- You may restrict the committee host name (by address, for instance) without
  touching the members site.

## Configuration

Everything is set through environment variables; `.env.example` lists them
with comments. The service refuses to start, naming the variable, when a
required one is missing or invalid. Business content lives in versioned files
embedded in the binary: `config/robots.yaml` (AI robots refused) and
`config/vpdive.yaml` (links to VPDive pages).

## Data protection

- Personal data (names, emails, imported VPDive fields) is encrypted with
  AES-256-GCM before it reaches SQLite. Keys derive from `SECRET_KEY`.
- At startup the service checks that `SECRET_KEY` decrypts the existing data
  and refuses to start otherwise.
- There is no key rotation. Changing `SECRET_KEY` makes existing data
  unreadable. Back the key up separately from the database.
- Logs and traces never contain a token, an email address, a name or a
  request body; spans are named after route patterns.

## Backup and restore

```sh
# Backup: a consistent copy made with SQLite's backup API, still encrypted.
docker compose exec app /sos-vpdive backup /data/backup-$(date +%F).db
docker compose cp app:/data/backup-$(date +%F).db .
```

Restore on a blank machine, with the same `.env` (same `SECRET_KEY`) and
`admins.yaml`:

```sh
docker compose stop app      # skip on a blank machine
docker compose run --rm -v "$PWD/backup-2026-10-05.db:/restore/backup.db:ro" app restore /restore/backup.db
docker compose up -d --wait
```

`restore` refuses a backup that `SECRET_KEY` cannot decrypt. Keep backups
30 days.

## Continuous integration

Every push and pull request runs gofmt, `go vet`, golangci-lint, the tests
with the race detector, a guard against committed spreadsheets, CSV files,
databases or `.env` files (only synthetic workbooks in `testdata/fixtures/`
are allowed), and an image build. No image is published.

## Dependencies

| Dependency | Why |
| --- | --- |
| `modernc.org/sqlite` | SQLite without CGO, so the binary is static and the image distroless |
| `golang.org/x/crypto` | argon2id password hashing |
| `golang.org/x/text` | Unicode normalization: name matching and spreadsheet headers |
| `golang.org/x/term` | `hash-password` reads a password without echo |
| `go.yaml.in/yaml/v3` | YAML content and accounts files (maintained successor of `gopkg.in/yaml.v3`) |
| `go.opentelemetry.io/otel`, `otel/trace`, `otel/sdk`, `otlptracehttp` | Traces over OTLP/HTTP, exported only when configured |
| `github.com/dicebear/dicebear-go/v10`, `github.com/dicebear/styles/v10` | Committee avatars generated offline (identicon style, CC0); they pull `github.com/dicebear/schema` and `github.com/santhosh-tekuri/jsonschema/v6` indirectly |
| `github.com/stretchr/testify` | Tests only |
| Tailwind CSS standalone CLI v4, daisyUI 5 (vendored `.mjs`) | Stylesheet built without Node or npm, checksums verified |
| Atkinson Hyperlegible Next | Self-hosted font, SIL Open Font License (`internal/web/static/fonts/OFL.txt`) |

## License

MIT
