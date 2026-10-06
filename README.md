# sos-vpdive

A small support desk for a volunteer-run diving club whose members use the
VPDive platform. Members file requests through a public form; a few committee
members handle them. One Go binary serves two host names: the members site
and the committee site. Simplicity and robustness beat features.

Status: lot 7 (Mollie collections and pushed imports): request form with
screenshots, fiches suggested before sending, tracking page and lost link,
committee board with live updates and summaries, assignment, internal notes,
journal, deletions and mails, both sites installable on a phone, committee
alerts by Web Push and Pushover, imports of the VPDive members, payments and
VPayDive exports, by hand or pushed by a script, with the requester's payments
and Mollie collections on each request, a list of cancelled outings and a list
of payments to check.

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
list, then the payments, on the Imports page. http://sos.localhost:8080 is the members site.

`make test` runs the tests, `make lint` the linter, `make css` the stylesheet.

Without `S3_*` variables, development stores screenshots under
`DATA_DIR/captures`. Mails go through a queue in the database to the SMTP
relay of `.env`: with an unreachable relay they stay queued, are retried, and
show on the committee's « Envois » page after 7 days.

## Deploy with Docker Compose

```sh
docker build --build-arg VERSION=$(git describe --tags --always) -t sos-vpdive:local .
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
  touching the members site. A script pushing the exports must then come from
  an allowed address, and the proxy must pass its `Authorization` header.
- The example compose file publishes the port on 127.0.0.1 only, so the proxy
  must run on the same host; with a remote proxy, change the binding and
  firewall the port so only the proxy reaches it.
- Do not buffer `/evenements` on the committee host name: it is a
  Server-Sent Events stream with a keepalive every 25 seconds. Keep the
  proxy's read timeout above 60 seconds (nginx: `proxy_buffering off;
  proxy_read_timeout 1h;`). Without the stream, the board still works and is
  refreshed by hand.
- Allow request bodies up to 16 MB on both host names: a request or a reply
  carries up to three 5 MB screenshots plus its fields, and the committee host
  takes import uploads (5 MiB plus fields), above nginx's 1 MB default
  (nginx: `client_max_body_size 16m;` in both server blocks).
- Serve HTTP/2 to browsers (nginx: `http2 on;`). The board and each request
  page hold a live stream open; over HTTP/1.1 six open tabs exhaust the
  browser's connection limit per host name.

## Configuration

Everything is set through environment variables; `.env.example` lists them
with comments. The service refuses to start, naming the variable, when a
required one is missing or invalid. Business content lives in versioned files
embedded in the binary: `config/robots.yaml` (AI robots refused),
`config/vpdive.yaml` (links to VPDive pages) and the fiches of `kb/`.

## Request categories and products

`config/categories.yaml` lists the categories of the form, their dedicated
fields (`text`, `textarea`, `choice`, `date`, `number`) and help texts;
`config/products.yaml` lists the products offered by `options_from: products`.
Ids are stable: a request keeps the ids in force when it was filed, and the
committee sees « retiré » next to a value whose field or option disappeared.
A category marked `committee_only` is never offered on the form; only a
reclassification leads to it. Both files are checked at startup.

## VPDive exports

The committee imports three VPDive exports on the Imports page, or a script
pushes them (next section); the service never connects to VPDive. Each upload
shows a preview, kept 15 minutes, and the confirmation replaces everything the
previous import stored.

- The members list (« Liste des membres » page, « Télécharger » button) is the
  whitelist of the form. The service keeps names, email, seasons and licence
  expiry, and reads no other column.
- The payments export (payments page, « Télécharger Excel » button) holds only
  what the page's filters show: set them as the Imports page says (display by
  members, every state, creation date over the last 24 months). The service
  keeps amounts, states, payment methods, titles and dates, and drops the
  names once hashed. It never reads comments, addresses or civility.
- A payment line reaches a request through the members list: the request's
  address, its member, then a hash of the normalised name. Lines whose name
  several members share are never shown, and stay hidden until the next
  payments import, even when a members import keeps only one of them.
- A request page shows carnet and training balances as VPDive reports them,
  never recomputed, the lines left to pay, the cancelled outings waiting for
  deletion and the ten latest lines. « Annulations » lists the outings whose
  title contains « annul », in any case, that still hold paid lines: the club
  renames a cancelled outing, refunds real-money payments, then deletes it in
  VPDive, which credits the carnets back.
- The VPayDive export (VPayDive page, « Exporter (Excel) » button, over the
  « Du » and « Au » dates) lists the payments Mollie collected, one line per
  cart item, and whether VPDive settled them. The service keeps the product,
  the outing and its date, the amount, « Payé », the payment date and the
  method. It never reads the commission, net amount, transfer, billing or API
  status columns. On a request page, « Encaissements Mollie » shows the
  requester's payments, newest first: the lines of one person at one minute
  make one payment. Every payment is made online, so the pages call the two
  exports « Paiements VPDive » and « Encaissements Mollie ».
- « À vérifier » lists the lines where the money received and the state in
  VPDive disagree: Mollie lines that VPDive did not settle (« Payé : Non »)
  and partial payments. The service fixes nothing. A resolver masks a line
  once checked; the line has no identifier, so the mask is keyed on a hash of
  the person, the date, the product and the amount, and survives later imports.
- `MEMBERS_MAX_AGE`, `PAYMENTS_MAX_AGE` and `VPAYDIVE_MAX_AGE` set when the
  committee is reminded to import again. Payment and Mollie lines are deleted
  after 90 days without an import of their export, the members list after 12
  months.

## Pushed imports

A script can push the three exports instead of a resolver, on a schedule for
instance. The script lives outside this repository: it signs in to VPDive, the
service never does. Set `IMPORT_TOKEN` to turn the route on (32 characters at
least, `openssl rand -base64 32`); without it, the route answers 404.

```sh
curl --fail-with-body -X POST -H "Authorization: Bearer $IMPORT_TOKEN" \
  --data-binary @export.xlsx https://comite.example.org/api/imports/payments
```

- `{type}` is `members`, `payments` or `vpaydive`. The body is the `.xlsx`
  file as VPDive produced it, 5 MB at most.
- The file goes through the same checks as an upload, without the preview: a
  valid file replaces the data in place in one transaction, and the journal
  names « script » as its author. A file with less than half of the data in
  place (accounts, or lines) is refused: upload it by hand if it is right.
- The answer is JSON: `{"result": "imported", "read": 120, "kept": 118,
  "skipped": 2, "to_check": 3}`, with `unchanged` when the file has the bytes
  of the latest import of its kind. A refusal answers `{"error": "<code>",
  "message": "…"}`: `unauthorized` (401), `rate_limited` (429),
  `unknown_type` (404), `too_large` (413), and 422 for a refused file with
  `invalid_workbook`, `too_many_rows`, `no_header`, `missing_column`,
  `invalid_number`, `invalid_date`, `empty_product`, `duplicate_email`,
  `invalid_email` or `too_few`.
- Every refused file mails the club inbox. The route takes 10 calls an hour
  per address and logs refused tokens, never the token itself.
- When an import outlives its maximum age, the club inbox gets one mail on top
  of the banner: with a script, an ageing import means the script is down.
- What the script does: download the members without filter, the payments and
  VPayDive over the last 24 months, push each file unchanged and keep none;
  when a download fails, push nothing.

## Knowledge base and suggestions

Each recurring problem has a fiche in `kb/<id>.md`: an answer for the member
and a procedure for the committee. The format:

```markdown
---
id: carnet-plongee-annulee
titre: Une plongée annulée a été décomptée de mon carnet
categories: [carnet, remboursement]
liens_vpdive: [paiements]
---

## Réponse adhérent

Short text shown to the member before sending.

## Procédure résolveur

1. Numbered steps in VPDive.
```

`categories` takes ids of `config/categories.yaml` and `liens_vpdive` keys of
`config/vpdive.yaml`. The text supports paragraphs, `- ` lists and `1. `
lists, nothing else. A fiche never quotes a price (link the club's price page
instead), a member's name or a secret: the repository is public. Fiches are
embedded at build time: changing one goes through a commit and a deployment.
`go run ./cmd/sos-vpdive validate-kb` runs the checks of the startup and lists
the `[À COMPLÉTER : …]` marks left to fill in; CI runs it too. A malformed
fiche refuses the start.

With `LLM_API_KEY` set, a sent form is stored as a draft and the model picks
up to three fiches; the member then sees their answers and either closes the
request (« Ça règle mon problème », counted on the committee's « Fiches » page)
or sends it anyway. The same call writes a summary of at most 200 characters
for the committee, shown on the board and the request page. The model never
writes to members: they only read fiches, and the server keeps only fiche ids
it knows. Without a key, or when the model fails or takes longer than
`LLM_TIMEOUT` (8 s), the request is sent at once and the board shows the start
of the description.

Any provider that speaks Anthropic's Messages API works:

| Provider | `LLM_BASE_URL` | `LLM_MODEL` |
| --- | --- | --- |
| Anthropic (default) | `https://api.anthropic.com` | `claude-haiku-4-5-20251001` |
| DeepSeek | `https://api.deepseek.com/anthropic` | `deepseek-flash` |

The model receives the category, the dedicated fields and the description,
never the name, the email address or the screenshots; the form says so next
to the description. Costs stay bounded: the anti-robot check, the rate limits
and the members list run before any call, the description is limited to
4 000 characters, the answer to 400 tokens, and `LLM_DAILY_LIMIT` (200 by
default) caps the calls per day, counted in Paris time.

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

## Installable apps

Each host name is also an app a phone can install: « SOS CPP » for members,
« SOS CPP Comité » for the committee, each with its own manifest, service
worker and icons. Chrome on Android offers to install it; on iPhone, Safari's
Share menu has « Sur l'écran d'accueil ». The service worker keeps the static
files and an offline page, nothing else: pages and screenshots always come
from the network. After a deploy, open pages show « Une nouvelle version du
site est disponible » and reload only when asked.

- On iPhone, the installed app keeps its own cookies and storage, apart from
  Safari: resolvers sign in again inside the app.
- Another club replaces the PNG files of `internal/web/static/icons/membres/`
  and `comite/` (192 and 512 px; a 512 px maskable one whose logo fits in the
  central 80 % circle; a 180 px Apple icon; a 32 px favicon), and the app
  names in `internal/web/pwa.go` and `templates/layout.html`.
- Browsers older than Chrome 111, Safari 16.4 or Firefox 128 get a short
  notice with the club's address instead of the page.
- The request form keeps what a member types in the browser's storage until
  the request leaves, 7 days at most, so a lost connection or a closed tab
  loses nothing. Screenshots and tokens are never kept there.

## Committee alerts

Besides the mail to the club mailbox, a new request and a member's reply can
reach the committee's phones. An alert carries the request reference and its
category, never a name or what the member wrote: it travels through Apple's,
Google's or Pushover's servers.

- Pushover: set `PUSHOVER_APP_TOKEN` to the token of an application created
  on pushover.net, and give each resolver who wants the alerts a
  `pushover_user_key` in the accounts file. Editing the file applies at once.
- Web Push, to the installed committee app: run `sos-vpdive vapid-keys`
  (`docker run --rm sos-vpdive:local vapid-keys`) once, copy both keys into
  `.env` and set `VAPID_SUBJECT=mailto:<club address>`. Keep the keys: new
  ones break every existing subscription. `PUSH_ALLOWED_HOSTS` lists the push
  services a phone may subscribe through. Each resolver then turns
  notifications on, device by device, on the « Notifications » page. On
  iPhone that works only from the installed app (iOS 16.4 or later). A
  subscription ends with its session: after signing in again, turn it back
  on there.

An alert is sent once, within the hour. If that fails, the log says so and
the mail still arrives. Alerts never show on the « Envois » page.

Before going live, check on a real Android phone and a real iPhone:

1. Install both apps; each opens on its own page, without the browser bar.
2. Turn notifications on in the committee app, file a request from the
   members app: the phone shows « Nouvelle demande » with the reference and
   the category only.
3. Refuse the permission on another device: the « Notifications » page says
   how to allow it.
4. Turn notifications off on the « Notifications » page, then on again;
   sign out: alerts stop reaching that device.

## Screenshot storage

Screenshots live in an S3-compatible bucket, Cloudflare R2 in production:

- Create the bucket in the EU jurisdiction (it cannot change later); the
  endpoint is `https://<account>.eu.r2.cloudflarestorage.com` with
  `S3_REGION=auto`.
- Keep it private: no public access, no custom domain. Scope the API token to
  object read and write on this bucket only.
- The service encrypts every screenshot before upload and serves it itself;
  browsers never get a bucket URL. A daily job removes objects left without a
  request for more than 24 hours. For that reason the bucket must hold
  nothing else and must not be shared between instances: that job deletes
  every object it does not recognise once it is older than 24 hours.
- `backup` covers the database only; screenshots stay in the bucket.

## Usage and errors

Both tools are optional and stay off until configured. An invalid value logs
a warning at startup and leaves that tool off; the service runs as before.

Umami counts page views. Set `UMAMI_SCRIPT_URL` to the script of your
instance, then one website ID per site: `UMAMI_WEBSITE_ID` for the members
site, `UMAMI_ADMIN_WEBSITE_ID` for the committee site. A site without an ID is
not measured, and without any ID Umami stays off. The instance must live on
another origin than both sites: on the same origin, the browser would send it
a tracking page's full address, token included, in the `Referer` header, so
such a URL is refused.

- A page is reported by its route, never by its address: a tracking page
  counts as `/suivi/[masqué]`, a request page as `/demandes/[id]`. The
  previous page and the title are not sent.
- With Do Not Track on, the browser never loads the script.
- Umami sets no cookie. The Content Security Policy allows its origin for
  the script and the page views, and for nothing else.
- No custom event is sent.

Sentry receives the server's errors, logs and traces when `SENTRY_DSN` is
set. It stays off with `APP_ENV=development`, even with a DSN.

- Every log line at error level becomes a Sentry issue, on the trace of its
  request: panics, 5xx answers, failed background jobs, notifications that
  failed for good. Expected refusals are not reported: invalid input, an
  address missing from the members list, rate limits, a postponed mail.
- Before an event leaves, the request (body, cookies, headers, IP address,
  URL) and the user are removed from it. Its message is the log line's, and
  logs never hold personal data.
- Traces are the OpenTelemetry spans, sent over OTLP to the DSN's project,
  always with the DSN's scheme: the `OTEL_EXPORTER_OTLP_*` variables of
  another collector do not apply to it.
- An unreachable Sentry slows no request. Nothing runs in the browser.

Events carry the build version. `make build` takes it from `git describe`;
for an image, pass `--build-arg VERSION=…` to `docker build`. The startup log
line shows it too.

## Data protection

- Personal data (names, emails, imported VPDive fields, request descriptions
  and fields, messages and committee notes, tracking tokens, queued mails) is
  encrypted with AES-256-GCM before it reaches SQLite. Keys derive from `SECRET_KEY`.
- At startup the service checks that `SECRET_KEY` decrypts the existing data
  and refuses to start otherwise.
- There is no key rotation. Changing `SECRET_KEY` makes existing data
  unreadable. Back the key up separately from the database.
- Screenshots are re-encoded on arrival (metadata dropped), encrypted the
  same way, then stored under random names.
- The model provider, when configured, receives the category, the dedicated
  fields and the description of each request, nothing else. The summary it
  writes is encrypted like the rest.
- Erasing a person deletes their requests, their members row and every
  payment and Mollie line carrying their name hash, a homonym's included. The
  next imports bring back what VPDive still holds.
- A masked line to check is stored as an HMAC of the line, without a name.
- Logs and traces never contain a token, an email address, a name or a
  request body; spans are named after route patterns. Sentry, when
  configured, receives these logs and traces and nothing more (see Usage and
  errors).
- Umami, when configured, receives page views reported by route, without
  the real address, the previous page or the title, and nothing from a
  browser with Do Not Track on.

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
with the race detector, `validate-kb` on the fiches, a guard against committed spreadsheets, CSV files,
databases or `.env` files (only synthetic workbooks in `testdata/fixtures/`
are allowed), and an image build. Once those pass, a push to develop publishes
`skynewz/sos-vpdive:latest` and a tag `vX.Y.Z` publishes `skynewz/sos-vpdive:X.Y.Z`
on Docker Hub, with the `DOCKERHUB_TOKEN` secret. A fork changes `IMAGE` and
the login username in `.github/workflows/ci.yml` and sets its own secret.

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
| `github.com/getsentry/sentry-go`, `sentry-go/otel`, `sentry-go/slog` | Optional error, log and trace reporting to Sentry: the official SDK, the link between its errors and the existing spans, and its `log/slog` handler |
| `github.com/dicebear/dicebear-go/v10`, `github.com/dicebear/styles/v10` | Committee avatars generated offline (Voxel Art style, CC0); they pull `github.com/dicebear/schema` and `github.com/santhosh-tekuri/jsonschema/v6` indirectly |
| `github.com/minio/minio-go/v7` | S3 client for the private screenshot bucket (Cloudflare R2, any S3-compatible store); it pulls `klauspost/compress`, `klauspost/cpuid`, `klauspost/crc32`, `minio/crc64nvme`, `minio/md5-simd`, `philhofer/fwd`, `rs/xid`, `tinylib/msgp`, `zeebo/xxh3` and `gopkg.in/ini.v1` indirectly |
| `github.com/stretchr/testify` | Tests only |
| Tailwind CSS standalone CLI v4, daisyUI 5 (vendored `.mjs`) | Stylesheet built without Node or npm, checksums verified |
| Atkinson Hyperlegible Next | Self-hosted font, SIL Open Font License (`internal/web/static/fonts/OFL.txt`) |

## License

MIT
