# sos-vpdive

A small help desk for a volunteer-run diving club whose members use VPDive.
Members ask for help through a public form, and a few committee members
answer. One Go binary serves two sites: one for members, one for the
committee.

- Members need no account. Before sending, the form shows the help fiches
  that may already answer them, and each request gets a private tracking link.
- The committee works from a live board: status, assignee, internal notes,
  history, and the requester's VPDive payments next to each request.
- New requests reach the committee by mail, and optionally by Web Push or
  Pushover. Both sites install as phone apps.
- The service never connects to VPDive. The committee imports VPDive's Excel
  exports by hand, or a script pushes them.

## Try it locally

You need Go (version in `go.mod`), `make` and `curl`.

```sh
cp .env.example .env
# In .env: APP_ENV=development, BASE_URL=http://sos.localhost:8080,
# ADMIN_BASE_URL=http://comite.localhost:8080, SECRET_KEY=$(openssl rand -base64 32),
# OWNER_USERNAME=alice, and empty TURNSTILE_* and S3_* values.
make account ARGS='-name Alice -role Présidente alice'   # prints a temporary password
make run
```

Sign in at http://comite.localhost:8080 with that password: the first sign-in
asks for a new one. Then import the members list on the Imports page. The
members site is http://sos.localhost:8080. Without `S3_*`, screenshots are
stored under `DATA_DIR/captures`.

`make test`, `make lint` and `make css` run the tests, the linter and the
stylesheet build.

## Deploy

```sh
docker build --build-arg VERSION=$(git describe --tags --always) -t sos-vpdive:local .
cp .env.example .env                                    # fill in every required value
docker compose up -d --wait
docker compose exec app /sos-vpdive reset-password -name "First name" -role "Function" <OWNER_USERNAME>
```

`.env.example` lists every variable with a comment. The service refuses to
start when a required one is missing or invalid, and names it.

The image is distroless and runs as a non-root user on a read-only file
system. The database lives in the `/data` volume. The owner
(`OWNER_USERNAME`) creates the other committee accounts on the « Comptes »
page, each with a temporary password to change at the first sign-in. If the
owner loses their password, this prints a new temporary one:

```sh
docker compose exec app /sos-vpdive reset-password <OWNER_USERNAME>
```

The image has no shell, so `exec` runs the binary itself.

### Reverse proxy

- Terminate TLS and forward both host names to port 8080 with the `Host`
  header unchanged.
- Set `X-Forwarded-For` and list the proxy's address, as the container sees
  it, in `TRUSTED_PROXIES`. For a proxy on the same host as the example
  compose file, the default `172.30.30.1/32` (the gateway of the network
  pinned in `compose.yaml`) is right.
- Never log the path or the `Referer` header of members-site requests.
  Tracking links carry a secret token, and a tracking page sends its own
  address as `Referer` with every file and form it loads. nginx's default
  `combined` log format records both.
- Do not buffer `/evenements` on the committee site, and keep the read
  timeout above 60 seconds (nginx: `proxy_buffering off; proxy_read_timeout 1h;`).
  It is a live stream with a keepalive every 25 seconds. Without it, the board
  still works but needs a manual refresh.
- Accept request bodies up to 16 MB on both sites (nginx:
  `client_max_body_size 16m;`). A request can carry three 5 MB screenshots.
- Serve HTTP/2. Every open committee tab holds a live stream, and HTTP/1.1
  allows only six connections per host name.
- The example compose file publishes the port on 127.0.0.1 only. For a proxy
  on another machine, change the binding and firewall the port.
- You can restrict the committee site, by IP address for example. A script
  that pushes exports must then connect from an allowed address, and the
  proxy must pass its `Authorization` header through.

## Adapt it to your club

What is specific to a club lives in versioned files. They are embedded in the
binary and checked at startup.

| File | Holds |
| --- | --- |
| `config/categories.yaml` | Form categories, their extra fields and help texts. A `committee_only` category never shows on the form. |
| `config/products.yaml` | The choices of a field with `options_from: products` |
| `config/vpdive.yaml` | Links to VPDive pages, shown to the committee |
| `config/robots.yaml` | AI crawlers that get a 403 |
| `config/calendar.yaml` | Labels of the calendar's categories, activities, places and roles as the import script pushes them, and a tint (1 to 5) per category. A value missing here shows as received. |
| `kb/*.md` | The help fiches (next section) |

Once ids are in use, keep them. A request keeps the ids it was filed with,
and the committee sees « retiré » next to a value that no longer exists.

App icons are PNG files in `internal/web/static/icons/membres/` and
`comite/`: 192 and 512 px, a 512 px maskable one with the logo inside the
central 80 % circle, a 180 px Apple icon and a 32 px favicon. The app names
are in `internal/web/pwa.go` and `templates/layout.html`. The members' icons
add the [SOS icon by Freepik from Flaticon](https://www.flaticon.com/free-icons/sos)
to the club logo, and its free licence asks for this credit.

Chrome's install dialog shows the screenshots in
`internal/web/static/screenshots/membres/` and `comite/`: `etroite.png`
(824 × 1830) and `large.png` (1280 × 800). A test checks those sizes. Take
them on a local instance filled with the synthetic files of
`testdata/fixtures/`, never with real requests.

### Fiches

Each recurring problem gets a fiche in `kb/<id>.md`, with an answer for the
member and a procedure for the committee:

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

`categories` takes ids from `config/categories.yaml`, and `liens_vpdive`
takes keys from `config/vpdive.yaml`. The text supports paragraphs, `- `
lists and `1. ` lists. The repository is public, so a fiche never quotes a
price, a member's name or a secret: link the club's price page instead.

A fiche ships with the binary, so changing one means a commit and a deploy.
`go run ./cmd/sos-vpdive validate-kb` runs the startup checks and lists the
`[À COMPLÉTER : …]` marks still to fill in. CI runs it too, and a malformed
fiche stops the service from starting.

## VPDive exports

The committee imports three exports on the Imports page. Each upload shows a
preview for 15 minutes, and confirming replaces what the previous import
stored.

| Export | Where in VPDive | Used for |
| --- | --- | --- |
| Members list | « Liste des membres », « Télécharger » | Who may use the form |
| Payments | Payments page, « Télécharger Excel », with the filters the Imports page gives | Payments on each request, « Annulations », « À vérifier » |
| VPayDive | VPayDive page, « Exporter (Excel) », over the « Du » and « Au » dates | « Encaissements Mollie » on each request, « À vérifier » |

The service reads only the columns it needs. It hashes the names on payment
lines and never reads addresses, comments or civility.
`MEMBERS_MAX_AGE`, `PAYMENTS_MAX_AGE`, `VPAYDIVE_MAX_AGE` and
`CALENDAR_MAX_AGE` set when the committee is reminded to import again.
Payment and Mollie lines are deleted after 90 days without a new import, the
members list after 12 months. Calendar events are deleted 12 months after
their start.

### Pushed imports

A script can push the exports on a schedule instead. The script lives
outside this repository: it signs in to VPDive, the service never does. Set
`IMPORT_TOKEN` (32 characters or more, `openssl rand -base64 32`) to turn the
route on. Without it, the route answers 404.

```sh
curl --fail-with-body -X POST -H "Authorization: Bearer $IMPORT_TOKEN" \
  --data-binary @export.xlsx https://comite.example.org/api/imports/payments
```

The type is `members`, `payments` or `vpaydive`. The body is the `.xlsx` file
exactly as VPDive produced it. The file goes through the checks of an upload,
without the preview, and the journal names « script » as its author. A file
holding less than half of the data in place is refused: upload it by hand if
it is right.

The type `calendar` takes the club's activity calendar as JSON, which only
the script produces: there is no manual upload. Each event is stored or
updated by its id; a stored event that starts within the pushed window and is
missing from the push is deleted, and older events stay as history. A
calendar holding less than half of the events stored in its window is
refused. A malformed calendar is refused with `invalid_calendar`.

The answer is JSON: `{"result": "imported", "read": 120, "kept": 118,
"skipped": 2, "to_check": 3}`, with `"unchanged"` when the file has the same
bytes as the latest import of its type. A refusal reads
`{"error": "<code>", "message": "…"}`:

| Status | Code |
| --- | --- |
| 400 | `interrupted`: the body was cut short |
| 401 | `unauthorized`: wrong token, logged without the token |
| 404 | `unknown_type` |
| 413 | `too_large`: body over 5 MB |
| 422 | The file is refused: `too_large` (over 50 MB once decompressed), `too_many_rows`, `invalid_workbook`, `no_header`, `missing_column`, `invalid_number`, `invalid_date`, `empty_product`, `duplicate_email`, `invalid_email`, `invalid_calendar` or `too_few` |
| 429 | `rate_limited`: 10 calls an hour per address |
| 500 | `internal` |

Each refused file (413 or 422) sends a mail to the club inbox. When an
import outlives its maximum age, the club inbox gets one mail as well: with a
script, it means the script has stopped working.

The script should download the members list without a filter and the two
payment exports over the last 24 months, push each file unchanged, keep no
copy, and push nothing when a download fails.

## Optional services

An invalid Umami or Sentry setting turns that tool off with a warning at
startup, and the service starts anyway.

### Fiche suggestions

With `LLM_API_KEY` set, a model picks up to three fiches to show the member
before sending, and writes a short summary for the committee. Members only
ever read fiches, never model output. The model receives the category, the
extra fields and the description, never the name, the email address or the
screenshots. Without a key, or when the model fails or takes longer than
`LLM_TIMEOUT` (8 s), the request leaves at once. `LLM_DAILY_LIMIT` (200 by
default) caps the calls per day.

Any provider that speaks Anthropic's Messages API works:

| Provider | `LLM_BASE_URL` | `LLM_MODEL` |
| --- | --- | --- |
| Anthropic (default) | `https://api.anthropic.com` | `claude-haiku-4-5-20251001` |
| DeepSeek | `https://api.deepseek.com/anthropic` | `deepseek-flash` |

Every suggestion call turns reasoning off. DeepSeek reasons by default and
would otherwise spend the 400-token answer budget before writing anything.

### Committee assistant

Off by default: set `ASSISTANT_ENABLED=true`, with `LLM_API_KEY`. It adds an
« Assistant » page to the committee site and an « Analyser » button on each
request. A resolver pastes a member's message or asks a question, and the model
answers from the club's data through read-only tools: the members list, VPDive
and Mollie payments, the calendar, the requests filed in the tool, cancelled
outings and the fiches. It changes nothing. The resolver acts in VPDive.

The model provider receives the resolver's text and the text of the request
being analysed, with email addresses, phone numbers and IBANs masked. Every
string a tool returns is masked the same way. The tools return member and
participant names, seasons and licence end, payment lines, outings and carts,
summaries of past requests and the fiches. One answer reads the payments,
outings and requests of three people at most; an outing it opens lists the
names of all its participants. Email addresses, phone numbers and IBANs are
masked before leaving. Screenshots and internal notes are never sent.

Conversations live in the server's memory only: 30 minutes after the last
question, 2 hours at most, and they go at logout, on an erasure or a
deletion, and on restart. Each account gets `ASSISTANT_DAILY_QUESTIONS`
questions a day (50 by default). The owner sees a usage journal at `/assistant/journal`: who asked,
when, tokens and an estimated cost (`ASSISTANT_PRICE_*`), never the questions.
`ASSISTANT_MODEL` picks the model (`LLM_MODEL` when empty) and
`ASSISTANT_THINKING` turns its reasoning on. The provider must support tool
use through the Messages API.

### Mail

Mail goes out through the SMTP relay of `SMTP_*`, always encrypted:
`SMTP_TLS=implicit` (port 465) or `SMTP_TLS=starttls` (port 587). Resend
works as is (host `smtp.resend.com`, user `resend`, the API key as
password). Check the SPF and DKIM records of `MAIL_FROM`'s domain before
going live.

Mails wait in a database queue and are retried for 7 days, so a relay outage
loses nothing. A mail that fails for good shows on the committee's
« Envois » page, where it can be sent again.

### Committee alerts

An alert reads « CPP-0042 · Léa Martin · Carnet, solde de plongées », followed
by the model's summary when there is one. Web Push alerts are encrypted for
the device, so Apple's and Google's servers cannot read them. Pushover gets
them in clear, like the club mailbox. An alert is sent once; if that fails,
the mail still arrives.

- Pushover: set `PUSHOVER_APP_TOKEN` to the token of an application created
  on pushover.net. Each resolver who wants alerts sets their user key on the
  « Notifications » page.
- Web Push: run `sos-vpdive vapid-keys` once and copy both keys into `.env`.
  Keep them, because new keys end every subscription. Set `VAPID_SUBJECT` to
  the club's bare address, `mailto:club@example.org`: the service refuses
  `mailto:<club@example.org>`, which Apple answers with a 403.
- Each resolver turns alerts on device by device on the « Notifications »
  page, which also sends a test notification. On iPhone this only works from
  the installed app, on iOS 16.4 or later.

### Screenshot storage

Production keeps screenshots in an S3-compatible bucket, such as Cloudflare
R2. Create the bucket in the EU jurisdiction (endpoint
`https://<account>.eu.r2.cloudflarestorage.com`, `S3_REGION=auto`), keep it
private and limit the API token to object read and write on that bucket.
Give the service a bucket of its own: a daily job deletes any object it does
not recognise once it is 24 hours old. Screenshots are encrypted before
upload, and only the service serves them.

### Usage and errors

Umami counts page views once `UMAMI_SCRIPT_URL` and a website ID per site are
set (`UMAMI_WEBSITE_ID` for members, `UMAMI_ADMIN_WEBSITE_ID` for the
committee). Host Umami on another origin than both sites: on the same origin,
the browser would hand it tracking tokens in `Referer`, so the service
ignores such a script URL. Pages are reported by route (`/suivi/[masqué]`), Umami
sets no cookie, and a browser with Do Not Track on never loads the script.

Sentry receives the server's errors, logs and traces when `SENTRY_DSN` is set
and `APP_ENV` is not `development`. Every error-level log line becomes an
issue, while expected refusals such as invalid input or rate limits do not.
Request data and the user are removed before an event leaves.

## Before going live

On a real Android phone and a real iPhone:

1. Install both apps. Each opens on its own page, without the browser bar.
2. Turn alerts on in the committee app, then file a request from the members
   app. The phone shows « Nouvelle demande » with the reference, the
   requester and the category, and a tap opens the request.
3. Refuse the permission on another device: the « Notifications » page says
   how to allow it.
4. Turn alerts off, then on again, then sign out: the device stops receiving
   them.

## Data protection

- Personal data is encrypted with AES-256-GCM before it reaches SQLite or the
  bucket. The keys derive from `SECRET_KEY`.
- The service checks at startup that `SECRET_KEY` decrypts the existing data,
  and refuses to start otherwise. There is no key rotation: with another key,
  the data is lost. Back the key up apart from the database.
- Logs and traces never hold a token, an email address, a name or a request's
  text.
- Suggestions send the model provider a request's category, extra fields and
  description, never the requester's name, email address or screenshots. With
  the committee assistant on, it also gets the resolver's masked text and the
  names and data the tools read. Email addresses, phone numbers and IBANs are
  masked before leaving; screenshots and internal notes are never sent (see
  « Committee assistant »).
- Erasing a person on the « Effacement » page deletes their requests, their
  member entry and every payment, Mollie line, calendar participation and
  unregistration under their name, a namesake's included. Where they
  unregistered someone else from an outing, their name is removed from that
  record. The next imports bring back what VPDive still holds.

## Backup and restore

`backup` makes a consistent copy of the database with SQLite's backup API. The
copy stays encrypted and does not hold the key. Screenshots are not in it:
they stay in the bucket.

```sh
docker compose exec app /sos-vpdive backup /data/backup-$(date +%F).db
docker compose cp app:/data/backup-$(date +%F).db .
```

To restore on a blank machine, bring the same `.env` (same `SECRET_KEY`), then:

```sh
docker compose stop app      # skip on a blank machine
docker compose run --rm -v "$PWD/backup-2026-10-05.db:/restore/backup.db:ro" app restore /restore/backup.db
docker compose up -d --wait
```

`restore` refuses a backup that `SECRET_KEY` cannot decrypt. Keep backups for
30 days. They pile up in the volume: remove old ones with
`docker run --rm -v <project>_data:/data busybox rm /data/backup-2026-09-05.db`,
or write them to a mounted host directory.

## Continuous integration

Every push to `main` or `develop` and every pull request runs gofmt,
`go vet`, golangci-lint, the tests with the race detector, `validate-kb`, an
image build, and a guard that fails on committed spreadsheets, CSV files,
databases or `.env` files (synthetic workbooks in `testdata/fixtures/` are
allowed). Once those pass, a push to `develop` publishes
`skynewz/sos-vpdive:latest` on Docker Hub, and a `vX.Y.Z` tag publishes
`skynewz/sos-vpdive:X.Y.Z`. A fork changes `IMAGE` and the login user in
`.github/workflows/ci.yml` and sets its own `DOCKERHUB_TOKEN` secret.

## License

MIT
