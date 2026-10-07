# Support VPDive CPP

Small support desk for a volunteer-run diving club: members file requests about
their club platform (VPDive), four committee members handle them. About 5
requests a day. Simplicity and robustness beat features.

## Source of truth

The spec is `.local/spec.md` (French, not in this repo). It was reviewed and
validated: it wins over this file, over existing code, and over preference.

- It opens with a reading guide. Work is split into lots (§15), one at a time.
- A lot is done when its §13 criteria pass.
- If the spec is ambiguous, contradictory, or looks wrong: stop and ask. Do not
  deviate silently.
- Open questions live in §14. They are not yours to resolve.

### Spec amendments decided by the user (they win over the spec text)

- `Referrer-Policy: same-origin` on every response, tracking pages included:
  `no-referrer` (§11.1) makes browsers send `Origin: null` on form posts, which
  the §11.2 Origin check refuses.
- CI publishes the image to Docker Hub (private `skynewz/sos-vpdive`) once the
  checks pass: develop as `:latest`, a tag `vX.Y.Z` as `:X.Y.Z` (§9.4).
- `sessions.credential_hash` (§8.2): a session is valid only while it matches
  the account's current password hash.
- Committee accounts live in the database, not in a file (§4.1, §8.2, §9.3,
  §10): `OWNER_USERNAME` names the owner, who creates, resets and deletes the
  other accounts on `/comptes` (404 for anyone else). A new account or a
  reset gets a temporary password shown once; the first sign-in leads every
  page to « Mon compte » until it is changed. Identifier, name and function
  never change. `reset-password` (run with `docker exec`) gives any existing
  account a new temporary password and creates a missing one: that is how the
  owner gets back in. `ADMINS_FILE` and `hash-password` are gone.
  Each resolver sets their Pushover key on « Notifications ».
- `.env.example` lists only the variables the binary reads; each lot adds its own.
- Pushover is per resolver: an optional `pushover_user_key` per account,
  set on « Notifications »; `PUSHOVER_APP_TOKEN` stays in the environment and
  `PUSHOVER_USER_KEY` is gone (§6, §10).
- A cancelled outing on the request page (§7.3) gets a note by payment method:
  « Prépayé » says the carnet is credited back when the outing is deleted, any
  other method says it was paid in real money and the treasurer refunds it.
- Erasing a person (§4.5) deletes every payment and Mollie line of their name
  hash, a homonym's included.
- Every payment is online: the VPayDive export is « Encaissements Mollie » on
  the committee side (§7.5, §7.7), its signal « encaissé par Mollie, non soldé
  dans VPDive : à vérifier », and the `vpaydive` method of the payments export
  reads « Mollie (VPayDive) ». Code keeps `vpaydive` and `online_payment_lines`.
- A pushed import (§7.6) under half of the data in place is refused for the
  three exports, not only the members list.
- Umami counts page views only (§9.10): no custom event, so neither the event
  table nor the « demande envoyée » criterion of §13.
- Sentry is fully off with `APP_ENV=development`, even with `SENTRY_DSN` set.
- An invalid `SENTRY_DSN` or `UMAMI_*` value turns that tool off with a startup
  warning instead of refusing to start (§10).
- Sentry also receives every log line. A record at `Error` or above is a Sentry
  error event: log levels decide what is reported (§9.10), so expected
  failures log at `Warn` or below.
- Owner's feedback after the first deploy (2026-10-07):
  - `/annulations` lists the outings newest first (§7.4).
  - The board filters apply as soon as they change; « Filtrer » stays. The
    phone list shows the category (§4.2).
  - Pushover and Web Push alerts read « CPP-0042 · Prénom Nom · Catégorie »,
    then the model's summary when there is one (§6, §9.6).
  - Icons are allowed in the committee navigation only (Lucide, always with
    their label); the neutrals are warm sand (§12.2). The rest of §12 holds.
  - Desktop committee pages show a toast for a change made by someone else;
    the SSE event carries `self` for the resolver's own changes (§4.2).
  - The suggestion call sends `"thinking": {"type": "disabled"}` (§5.2).
- The README has no dependency list (§9.1, §9.3): the commit that adds a
  dependency says why.
- The club's activity calendar is in scope (§2, §7.6, lot 8): the external
  script pushes it as JSON to `POST /api/imports/calendar`; the tool still
  never contacts VPDive.
  - Each event is stored or updated by its `id`. A stored event that starts
    in `[from, to]` (Paris dates) and is missing from the push is deleted;
    the others stay as history, 12 months after their start (§8.3).
  - The « under half » guard compares with the events stored in
    `[from, to]`. There is no manual upload, so no way past a `too_few`
    refusal in v1.
  - `CALENDAR_MAX_AGE` defaults to 48 h (§10).
  - Matching (§7.3): by `name_hash` only for a registered participant with
    both a last and a first name; otherwise by `vpdive_id`, through another
    registered participation. Never on an empty name.
  - Erasing a person (§4.5) deletes their participations, a homonym's
    included: by `name_hash`, by the full name of an unregistered
    participant in either order (erasure only, never for matching), and by
    the `vpdive_id` of those.
  - Display (lot 8 part 2), committee only, read-only: `/calendrier`
    (month, week, day; week by default) and `/calendrier/{id}`, in the
    sidebar and « Plus ». Phones get day lists instead of the month grid.
  - The ticket page's « Sorties VPDive » block lists the requester's outings
    (90 days before the request and every upcoming one; older ones folded),
    through the member's name_hash and the linked vpdive_id. A VPDive or
    Mollie line of the requester attaches to their outing of the same Paris
    day, by normalised title when there are several; the others stay in
    the existing blocks. Signals: cancelled outing with a paid « Prépayé »
    line, or paid in real money; Mollie line not settled. Roles raise no
    signal: everyone pays, some 0 €.
  - Category tints (§12.2): up to five muted tints of our own, set in
    `config/calendar.yaml` with the labels of category, activity,
    environment and role. VPDive's colours are ignored; an unknown value
    shows as received.
  - An `ends_at` before `starts_at` shows the start alone, and the outing
    page says « Heure de fin incohérente dans VPDive ».
  - Decryption stays per page: the month decrypts events only, week and day
    add their participants, the outing page its own; member matching counts
    rows by name_hash; nobody else's payment lines are read outside
    `/annulations`.
  - `/annulations` links an outing to its calendar page when title and
    Paris day match one event, otherwise to the day view.

## Private material: `.local/` is gitignored

`.local/` holds the spec and the real VPDive exports (`.local/exports/*.xlsx`).

- Nothing from `.local/` is ever committed, and none of its content is copied
  into code, tests, fixtures, docs, or commit messages: no member names,
  emails, amounts, club figures, committee first names, vendor ticket numbers.
- Real exports are read-only: for checking a parser against reality and for
  the private acceptance run. Tests use synthetic workbooks in
  `testdata/fixtures/`.
- This repo is public. README and docs are written as if for another club
  reusing the tool.

## Stack, fixed by the spec

- Go, standard library first (`net/http`, `html/template`, `log/slog`).
- SQLite with a pure-Go driver, no CGO. Single binary, single instance.
- Server-rendered HTML. Vanilla JS only where the spec requires it.
- Tailwind CSS 4 + daisyUI 5, built with the Tailwind standalone CLI. No Node
  in the build.
- Distroless image, non-root. One binary serves two hostnames.
- Config through environment variables only (§10). Business content in
  versioned YAML and Markdown (§9.7).
- Few dependencies; a new one is justified in the commit that adds it.

## Language

Code, identifiers, comments, commits, README: English.
UI strings, emails, knowledge-base articles: French, informal "tu" (§12.1).
No i18n framework.

## Invariants

- No personal data in logs or trace spans. Route templates, never raw paths:
  tracking links carry a secret token.
- Personal data is encrypted before it reaches SQLite or object storage (§8.4).
- No state change on GET. `Origin` is checked on every mutation (§11.2).
- The public hostname never serves committee routes, and vice versa (§9.6).
- The tool never connects to VPDive and never sends anything to its vendor.
- Model output is never shown to members: it only selects articles and writes
  a summary for the committee (§5).
- Design constraints of §12 override the defaults of any design skill.

## Out of bounds

- `.env`, database files, or `.xlsx` outside `testdata/fixtures/` in git.
- Anything the spec lists as out of scope (§2).
- Key rotation, an ORM, a JS framework, a second datastore.

## Architecture

- `cmd/sos-vpdive serve` builds everything in `setup` (serve.go): one SQLite
  handle, `secure.Keys` derived from `SECRET_KEY` (sealing, HMAC hashes,
  CSRF), the stores, then `web.Server`.
- `web.Server.route` picks the site by `Host`: the public mux (form, tracking)
  or the committee mux (`signedIn` routes, `/api/imports` by token). Both share
  `commonRoutes` (health, static, manifest, service worker).
- `tickets.Store` owns request state changes; it writes mails and alerts to
  the `mail.Outbox` in the same transaction, and `Outbox.Run` delivers them per
  channel (SMTP, Pushover, Web Push). Its `OnChange` feeds the SSE `Broker`.
- Background jobs in `serve`: outbox delivery, accounts reload, daily purge
  (`app.purge`, spec §8.3).

## Layout and commands

- `cmd/sos-vpdive` (subcommands) + `internal/{config,secure,telemetry,store,xlsx,imports,members,payments,calendar,admins,tickets,mail,blobs,images,kb,suggest,push,web}`.
  Migrations: `internal/store/migrations/NNNN_*.sql`. Content files `config/*.yaml`
  (categories, products, vpdive, robots) are embedded by the root `content.go`.
- `make test` / `make lint` (golangci-lint v2, `default: all`) / `make css` /
  `make build` / `make fixtures` (regenerates `testdata/fixtures/*.xlsx`).
- One test: `go test ./internal/web -run TestName` (`make test` adds `-race`).
- CI (`.github/workflows/ci.yml`): `check` runs `gofmt -l`, `go vet`,
  `go test -race`, `validate-kb` and the forbidden-files guard; `image` builds
  the Dockerfile and pushes it (develop → `:latest`, tag `vX.Y.Z` → `:X.Y.Z`).
- `make run` needs `.env` (from `.env.example`, `APP_ENV=development`); sites
  on `http://sos.localhost:8080` and `http://comite.localhost:8080` (browsers
  treat `*.localhost` as secure).
- `make account ARGS='…'` creates a local account.
- `./scripts/check-forbidden-files.sh` is the CI guard on private files.
- The repo's `.env`, `admins/` and `data/` are the owner's dev files: never delete
  or overwrite them; acceptance runs use a scratch `DATA_DIR`.
- Acceptance instance: binary built into a scratch dir, port 8091, started with
  `exec` after saving `$$`, stopped by that PID only (`make run` has the same
  command line: never `pkill`).
- `node --check internal/web/static/app.js` catches JS syntax errors (Node is a
  local convenience, never a build step).

## Workflow

- Gitflow, `feature/*` from `develop`. Greenfield: no PR, merge `--no-ff` into
  `develop` and push once everything is green; `main` is a release decision.
- Before merging a lot: `/simplify`, `/ponytail:ponytail-review`, `/codex:review`
  (only the user can run it), and a browser check of every page with a form
  (`playwright-cli`, or the desktop app's built-in browser when cheaper).
- Plans and designs go in `docs/superpowers/` (gitignored, never committed).

## Gotchas

- Tests that set `Origin` by hand cannot catch browser behaviour: verify forms
  in a real browser.
- Write invisible characters in Go tests as escapes (`\u00a0`, `\u0301`):
  editors and agents normalise raw ones away.
- html/template outputs `+` as `&#43;` and `'` as `&#39;`.
- `internal/web` tests take about 90 s (argon2id at 64 MiB per login).
- SQLite reuses the highest `INTEGER PRIMARY KEY` after a delete: ids that leave the
  process (URLs, in-flight sends) need `AUTOINCREMENT`.
- Migrations are tracked by number only: never edit one that has shipped.
- SQLite cannot alter a `CHECK`: rebuild the table and every table pointing at
  it, each new one filled before the old ones go (`defer_foreign_keys` does not
  survive the rename; see `0006_mollie.sql`).
- Go templates: no `{{else if}}` after `{{with}}` (nest instead); a map keyed by a
  named string type cannot be indexed with a literal (use `map[string]…`).
- Tailwind scans only `internal/web/templates`: `app.js` toggles attributes
  (`hidden`, `data-*`), never classes.
- Never disable the clicked button in a submit handler: its `name=value`
  (`action=…`) leaves the form data.
- The SMTP sender refuses plaintext and untrusted certificates: test mail end to end
  with Mailpit in TLS mode, its CA given to the container through `SSL_CERT_FILE`.
- Object keys never reach logs: `internal/blobs` scrubs its errors at the boundary;
  keep new store methods behind it.
- daisyUI control height is `--size-field` × 10: the theme sets 0.275rem for 44 px.
- Backups are switched to a rollback journal: a WAL-flagged copy cannot be opened
  read-only.
- The Dockerfile's CSS stage downloads Tailwind from GitHub; the CI `image` job
  is its end-to-end check.
- iOS installed apps: no `viewport-fit=cover`, no `env(safe-area-inset-*)`
  (iOS keeps the status bar and home indicator areas), and form controls at
  16 px or Safari zooms on focus (`--font-size-min` in `css/input.css`).
- Safari asks for push permission only from a direct tap: `pushManager.subscribe`
  comes first in the handler, and every push must show a notification.
- `VAPID_SUBJECT` is a bare `mailto:club@example.org`: Apple answers 403
  `BadJwtToken` to `mailto:<...>`, and the config refuses it.
- An installed app on iOS has one window: `clients.openWindow` only wakes it.
  The service worker posts `{open: url}` to the open window and `app.js` goes
  there unless a POST form field was changed.
- `.panel` (white block on the sand page) lives in `@layer components`, so a
  utility such as `border-primary` overrides its border.
- The service worker's cache name hashes every embedded file: any deploy that
  changes the site shows the update banner.
- Playwright's `route()` misses requests a service worker makes: to drop a
  response, open a context with `serviceWorkers: 'block'`.
- Headless Chromium cannot subscribe to a real push service: deliver a push with
  CDP `ServiceWorker.deliverPushMessage` and read `registration.getNotifications()`.
  `Page.getInstallabilityErrors` needs a persistent profile.
- The form takes 5 sends an hour per address (20 per IP): repeated browser runs
  need several of the six `members_valid.xlsx` addresses.
- Go 1.27 composite literals name promoted fields directly (`Info{Kind: k}`):
  the `modernize` linter asks for it on embedded structs.
- Payment lines are reached through the members list only (name hash), never
  through the name typed on the form (§7.3).
- Close response bodies through a named error return (`suggest`, `turnstile`,
  `push`); never `_ =` an error.
- Umami drops page views from a `HeadlessChrome` user agent (answers « beep
  boop »): give browser checks that read Umami's API a regular Chrome user agent.
- sentry-go ≥ 0.47 has no `EnableLogs`: logs flow once `WithSentry` wraps the
  logger. Its `otel/otlp` exporter inherits `OTEL_EXPORTER_OTLP_*` (an http
  endpoint downgrades it): `telemetry.sentryExporter` sets the URL with
  `WithEndpointURL` instead. A Sentry client with a custom `Transport` (tests) skips the telemetry
  buffer and delivers logs and events to it on `Flush`.
