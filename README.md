# sos-vpdive

A small support desk for a volunteer-run diving club whose members use the
VPDive platform. Members file requests through a public form; a few committee
members handle them. One Go binary serves two host names: the members site
and the committee site. Simplicity and robustness beat features.

Status: lot 2 (complete support, without the model): request form with
screenshots, tracking page and lost link, committee board with live updates,
assignment, internal notes, journal, deletions and mails. Knowledge-base
suggestions come next.

## Run it locally

Requirements: Go (see `go.mod`), `make`, `curl`.

```sh
cp .env.example .env
# In .env: APP_ENV=development, BASE_URL=http://sos.localhost:8080,
# ADMIN_BASE_URL=http://comite.localhost:8080, SECRET_KEY=$(openssl rand -base64 32),
# and empty TURNSTILE_* and S3_* values.
go run ./cmd/sos-vpdive hash-password        # type a password twice
mkdir -p admins && cp admins.example.yaml admins/admins.yaml   # paste the hash
make run
```

Open http://comite.localhost:8080 (browsers resolve `*.localhost` to your
machine and accept the `__Host-` session cookie there) and import the members
list on the Imports page. http://sos.localhost:8080 is the members site.

`make test` runs the tests, `make lint` the linter, `make css` the stylesheet.

Without `S3_*` variables, development stores screenshots under
`DATA_DIR/captures`. Mails go through a queue in the database to the SMTP
relay of `.env`: with an unreachable relay they stay queued, are retried, and
show on the committee's « Envois » page after 7 days.

## Deploy with Docker Compose

```sh
docker build -t sos-vpdive:local .
cp .env.example .env                                    # fill every required value
docker run --rm -it sos-vpdive:local hash-password      # once per account
mkdir -p admins && cp admins.example.yaml admins/admins.yaml   # paste the hashes
docker compose up -d --wait
```

The image is distroless (`gcr.io/distroless/static-debian13:nonroot`): no
shell, user 65532, read-only root file system. The database lives in the
`/data` volume; the `admins/` directory is mounted read-only at `/config`
(a directory rather than the file, so that editors saving by rename are
picked up by the hot reload).

### Behind a reverse proxy

- Terminate TLS at the proxy and forward both host names to port 8080 with the
  `Host` header unchanged.
- Set `X-Forwarded-For` and list the proxy's address, as the container sees it,
  in `TRUSTED_PROXIES`; otherwise the visitor address is the proxy's. With the
  example compose file, a proxy on the host reaches the container through the
  Docker bridge gateway: the default `172.30.30.1/32` is the gateway of the
  network pinned in `compose.yaml`. With another topology, list the address
  the proxy connects from.
- Do not log request paths of the members site: tracking links carry a secret.
- You may restrict the committee host name (by address, for instance) without
  touching the members site.
- The example compose file publishes the port on 127.0.0.1 only, so the proxy
  must run on the same host; with a remote proxy, change the binding and
  firewall the port so only the proxy reaches it.
- Do not buffer `/evenements` on the committee host name: it is a
  Server-Sent Events stream with a keepalive every 25 seconds. Keep the
  proxy's read timeout above 60 seconds (nginx: `proxy_buffering off;
  proxy_read_timeout 1h;`). Without the stream, the board still works and is
  refreshed by hand.

## Configuration

Everything is set through environment variables; `.env.example` lists them
with comments. The service refuses to start, naming the variable, when a
required one is missing or invalid. Business content lives in versioned files
embedded in the binary: `config/robots.yaml` (AI robots refused) and
`config/vpdive.yaml` (links to VPDive pages).

## Request categories and products

`config/categories.yaml` lists the categories of the form, their dedicated
fields (`text`, `textarea`, `choice`, `date`, `number`) and help texts;
`config/products.yaml` lists the products offered by `options_from: products`.
Ids are stable: a request keeps the ids in force when it was filed, and the
committee sees « retiré » next to a value whose field or option disappeared.
A category marked `committee_only` is never offered on the form; only a
reclassification leads to it. Both files are checked at startup.

## Mails

Mails leave through the SMTP relay of `SMTP_*`, always encrypted before
authentication: implicit TLS (`SMTP_TLS=implicit`, port 465) or STARTTLS
(`SMTP_TLS=starttls`, port 587). Resend works without dedicated code
(host `smtp.resend.com`, user `resend`, the API key as password). Verify the
domain of `MAIL_FROM` (SPF, DKIM) before going live.

Every notification is written in the database with the event that causes it,
then sent by a background worker: retries after 1 minute, 5 minutes,
30 minutes, 2 hours, 12 hours, then every 24 hours. After 7 days, or on a
definitive refusal, the mail is marked failed and listed on the committee's
« Envois » page, where it can be sent again.

## Screenshot storage

Screenshots live in an S3-compatible bucket, Cloudflare R2 in production:

- Create the bucket in the EU jurisdiction (it cannot change later); the
  endpoint is `https://<account>.eu.r2.cloudflarestorage.com` with
  `S3_REGION=auto`.
- Keep it private: no public access, no custom domain. Scope the API token to
  object read and write on this bucket only.
- The service encrypts every screenshot before upload and serves it itself;
  browsers never get a bucket URL. A daily job removes objects left without a
  request for more than 24 hours.
- `backup` covers the database only; screenshots stay in the bucket.

## Data protection

- Personal data (names, emails, imported VPDive fields) is encrypted with
  AES-256-GCM before it reaches SQLite. Keys derive from `SECRET_KEY`.
- At startup the service checks that `SECRET_KEY` decrypts the existing data
  and refuses to start otherwise.
- There is no key rotation. Changing `SECRET_KEY` makes existing data
  unreadable. Back the key up separately from the database.
- Screenshots are re-encoded on arrival (metadata dropped), encrypted the
  same way, then stored under random names.
- Logs and traces never contain a token, an email address, a name or a
  request body; spans are named after route patterns.

## Backup and restore

```sh
# Backup: a consistent copy made with SQLite's backup API, still encrypted.
docker compose exec app /sos-vpdive backup /data/backup-$(date +%F).db
docker compose cp app:/data/backup-$(date +%F).db .
```

Restore on a blank machine, with the same `.env` (same `SECRET_KEY`) and
`admins/admins.yaml`:

```sh
docker compose stop app      # skip on a blank machine
docker compose run --rm -v "$PWD/backup-2026-10-05.db:/restore/backup.db:ro" app restore /restore/backup.db
docker compose up -d --wait
```

`restore` refuses a backup that `SECRET_KEY` cannot decrypt. Keep backups
30 days. Backups pile up in the volume: remove old ones with
`docker run --rm -v <project>_data:/data busybox rm /data/backup-2026-09-05.db`,
or write them to a mounted host directory instead.

## Continuous integration

Every push to main or develop and every pull request runs gofmt, `go vet`, golangci-lint, the tests
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
| `golang.org/x/image` | WebP decoding: screenshots are re-encoded to drop their metadata, and the standard library reads no WebP |
| `go.yaml.in/yaml/v3` | YAML content and accounts files (maintained successor of `gopkg.in/yaml.v3`) |
| `go.opentelemetry.io/otel`, `otel/trace`, `otel/sdk`, `otlptracehttp` | Traces over OTLP/HTTP, exported only when configured |
| `github.com/dicebear/dicebear-go/v10`, `github.com/dicebear/styles/v10` | Committee avatars generated offline (Voxel Art style, CC0); they pull `github.com/dicebear/schema` and `github.com/santhosh-tekuri/jsonschema/v6` indirectly |
| `github.com/minio/minio-go/v7` | S3 client for the private screenshot bucket (Cloudflare R2, any S3-compatible store); it pulls `klauspost/compress`, `minio/md5-simd`, `minio/crc64nvme`, `rs/xid`, `tinylib/msgp`, `zeebo/xxh3` and `gopkg.in/ini.v1` indirectly |
| `github.com/stretchr/testify` | Tests only |
| Tailwind CSS standalone CLI v4, daisyUI 5 (vendored `.mjs`) | Stylesheet built without Node or npm, checksums verified |
| Atkinson Hyperlegible Next | Self-hosted font, SIL Open Font License (`internal/web/static/fonts/OFL.txt`) |

## License

MIT
