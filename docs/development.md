# Development Reference

Read the sections relevant to the task. `AGENTS.md` contains shared contributor
rules; `README.md` documents configuration and operation. This file preserves
domain constraints and implementation pitfalls without loading them every session.

## Scope and Sources

The user's current request defines the task. The private `.local/spec.md`
explains the original design; it is historical context, not an approval gate.
Name an intentional departure briefly rather than requiring a spec amendment.
`docs/adr/` records dated decisions; verify them against current code.
Local plans and designs belong in gitignored `docs/superpowers/`; do not add new
ones to Git. Already tracked historical documents are not current requirements.

Keep the single-instance Go/SQLite service, pure-Go driver, server-rendered
templates and distroless non-root image. Prefer the standard library and existing
helpers. An ORM, JavaScript framework, second datastore or key rotation is outside
routine changes and requires explicit scope. Runtime configuration uses environment
variables; versioned YAML and Markdown hold business content. New dependencies
need a reason in their introducing commit, not a README dependency inventory.

## Architecture and Security Boundaries

`cmd/sos-vpdive/serve.go` wires one SQLite handle, keys derived from `SECRET_KEY`,
stores and `web.Server`. The request host selects the public or committee router;
each must refuse the other site's private routes. Common routes serve health,
assets and PWA resources. Account polling, outbox delivery and retention run as
background jobs.

`tickets.Store` owns state transitions and enqueues mail/alerts in the same
transaction. `mail.Outbox` delivers SMTP, Pushover and Web Push; ticket changes feed
the SSE broker. Preserve transaction boundaries and retry behavior.

Never put personal data, tracking tokens or object keys in logs or spans. Record
route templates, not raw paths. Encrypt personal data before SQLite or blob
storage; keep object-store errors behind `internal/blobs` sanitization. Browser
mutations require Origin validation; import APIs authenticate with bearer tokens
instead. Do not introduce state-changing GET actions. Preserve
`Referrer-Policy: same-origin`: `no-referrer` breaks form Origin validation.

Committee accounts live in SQLite. `OWNER_USERNAME` controls owner-only account
management; temporary passwords require a password change. Sessions must match
the current credential hash. `reset-password` recovers or creates an account;
`ADMINS_FILE` and `hash-password` are obsolete. Pushover user keys belong to
individual accounts; the app token belongs to the environment.

## Chat éphémère du comité

Le salon commun est réservé aux comptes connectés en vue ordinateur (64 rem
au minimum). Il partage `/evenements` avec les demandes ; seuls les flux qui
optent pour `chat=1` reçoivent son contenu. Les messages sont envoyés en JSON
par `POST /chat/messages`, avec session, Origin et jeton anti-CSRF.

Aucun message n’est archivé en base ou dans un fichier. Le serveur garde au
plus 256 messages pendant 60 secondes pour reprendre une réception interrompue.
Un nouvel arrivant sans curseur ne reçoit pas de passé. Les onglets possèdent
leur historique en clair dans `sessionStorage`, lié à la session authentifiée :
masquer le panneau ou naviguer le conserve ; terminer la session le retire.
La reprise vérifie l’identité exacte de la session du document, y compris après
un retour arrière. Une erreur de lecture de session produit 503 et ne doit
jamais être assimilée à une révocation confirmée.
La duplication/restauration native d’onglet peut recopier ce stockage. Un
stockage indisponible fait basculer en mémoire avec un avertissement.

La présence est dédupliquée par compte, avec 30 secondes de grâce pour un départ
ordinaire. Une révocation est immédiate. En vue mobile, le chat est désactivé ;
le SSE des demandes garde son fonctionnement. Les non-lus et le choix muet
restent propres à chaque onglet. Le son requiert une interaction autorisée
par le navigateur. Aucun contenu du salon ne rejoint les journaux, traces,
services d’IA ou notifications système.

## Private Data and Acceptance Runs

This repository is public: write reusable documentation for another club.
`.local/` holds private specifications and real exports. Never copy its names,
addresses, amounts, club figures or vendor ticket references into tracked content,
including fixtures and commit messages. Real exports are read-only, used only for
private parser checks or acceptance. Automated tests use synthetic fixtures.

Preserve the owner's `.env`, `admins/` and `data/`. Use an isolated scratch
`DATA_DIR` and binary for acceptance, conventionally port 8091. Start with `exec`,
record the exact PID and stop only that process; never use `pkill`.
`make run` explicitly sets `DATA_DIR=./data`, so it is unsuitable for an isolated
acceptance run. Model benchmarks make billed API calls: obtain approval first.

## Imports, Identity and Erasure

Manual exports and the separate fetcher feed imports. Preserve the under-half
replacement guard and refusal journaling; calendar comparisons cover only the
pushed time window.
Import runs retain 90 days, seal failure details and return `recorded: true` for a
journaled refusal. Script-reported failures send no mail; staleness alerts cover
lasting failures. Preserve existing refusal emails.

Payment access uses a member's `name_hash`, never the name typed on the public
form. Calendar matching uses a registered person's complete name hash, or a
`vpdive_id` already linked by a registered participation; never match empty names.
Unregistrations match by complete name hash only, since they have no upstream
identifier.
Erasure deliberately includes homonyms: delete payment/Mollie lines and calendar
participations/unregistrations for the erased identity. Handle unregistered names
in both orders for erasure only and blank matching unregistration authors.
Extend existing erasure and retention paths when introducing personal data.

Campaign parsing uses a strict allowlist: Statut, Nom, Prénom, validation date and
author, Adhésion, License and Règlement. Never read or retain raw validation text,
age, section, CACI health data, licence numbers, cards, training or documents.
`/adhesions`, like `/anomalies` and `/annulations`, is a deliberate committee-wide
data view; do not generalize those exceptions to ordinary member lookups.

A campaign block may match the typed name in either order for a requester absent
from the member list, marked “à confirmer”; that fallback must never authorize
payment access, member disclosure or mail. A campaign-only person has no email:
erasure happens upstream, through replacement imports or retention.
Campaign matches hide partial member matches; duplicate campaign names represent
one ambiguous identity and must not unlock payments or outings. Preserve the
restrictive acknowledgement conditions in `internal/tickets` and their tests.

## Calendar, Payments and Knowledge Base

Calendar imports update by event ID and replace missing events only in the pushed
Paris-date interval. Other history stays until its 12-month retention expires.
Keep Paris-day boundaries consistent across matching, filtering and display.
An inverted end time displays only the start with a warning. Decrypt only what
the page needs: month events, week/day participants, or the opened outing.
Unregistrations appear on the requester's ticket, not ordinary calendar views.

Attach imported payment lines to outings by normalized title and Paris day;
a Mollie product line may attach to the person's sole outing that day. Preserve
unmatched lines. Cancellation notices distinguish prepaid credit from real-money refunds.
Roles alone never indicate a payment anomaly; some participants owe zero.
Mollie handles online payments; preserve existing internal identifiers.

`kb/tarification.md` is the pricing source, including active-member rules.
Do not duplicate prices in prompts or code. Public tariff content takes priority
over inconsistent upstream price plans. Calendar labels and muted colors live in
`config/calendar.yaml`; ignore upstream colors and display unknown labels as
received.
Preserve the house theme, club navy, warm sand and self-hosted font. Labeled Lucide
icons belong in committee navigation; follow explicit user requests for UX changes.

## Model Services

Current configuration is in `.env.example` and `internal/config/config.go`:
`SUGGEST_*` selects one known fiche or none and suggests a public category in
one OpenAI Decisions call;
`SUMMARY_*` independently generates a committee summary; `ASSISTANT_*` configures
the committee assistant. `LLM_*` is obsolete. Avoid duplicating model names,
prices or provider-specific settings here; README covers the current contracts.

Suggestions send article IDs/indexes and request category, values and description,
never article bodies, resolver procedures, identity fields or screenshots.
Suggestion and summary descriptions are unmasked; do not claim otherwise.
Public category IDs, labels and field labels are also supplied as choices.
Validate fiche and category answers independently; keep a valid answer when the
other is invalid, but journal the combined call as invalid_output. The category
cutoff is provisionally 0.90; fiche selection retains its own cutoff. Persist
category advice sealed, committee-only, and resolve it durably on manual
classification, dismissal or closing. A late answer must not revive resolved
advice; adding late advice bumps the ticket version and notifies after commit.
Never recalculate advice on page loads or backfill historical tickets.
Failures, refusals or quotas must never prevent submitting a request. Members see
authored fiche text only; generated summaries and assistant output stay committee-only.

The assistant is off by default. Its tools are read-only; member-specific payment,
outing, request and membership tools share a three-person limit. An opened outing
can list all participants. Campaign-only identities have no request email;
ambiguous identities must not expose payments or outings. `member_membership`
counts toward the same person limit. Closed tickets refuse analysis in both UI
and API before a provider call.

Conversation archives and provider context are encrypted in SQLite and owned by
an account, retained for seven days from creation. List queries decrypt titles
only; visible archive reads do not load provider context. Resume only within
30 minutes of the last successful answer and two hours from creation, under the
20-question/256 KiB context limits. Reads and refused/failed questions must not
extend the window. An expired conversation is read-only; a new one copies no
context. Reconnection/restart must not reset deadlines. Logout/revocation cancel
live answers while saved history stays private. Global content erasure/deletion
must remove archives transactionally, including ticket retention; late response
or title updates must never recreate removed rows. One SUMMARY title attempt
uses the masked first question asynchronously, with a dated fallback and existing
usage accounting. Never send the transcript to generate a title.
Mask email, phone and bank patterns in input and each decoded tool-result string
with `assistant.Mask`/`MaskJSON`; masking encoded JSON misses escaped boundaries.
Render answers only through `assistant.Render`, neutralizing links and images.
Never send screenshots or internal notes. Usage journals contain metadata, never
conversation or request text; show monetary totals only when every call is priced.

## Verification and Tooling

Avec Node disponible, `go test` exécute aussi le contrôle déterministe du cycle
de vie du client chat, sans paquet tiers. Ce contrôle est ignoré si Node est
absent ; les builds restent indépendants de Node et l’acceptance UI utilise
toujours le véritable navigateur et son EventSource.

CI runs the forbidden-files guard, gofmt, vet, golangci-lint v2, `validate-kb` and
`go test -race ./...`. Image builds run after checks on `develop` and version tags,
not PRs: Dockerfile changes need a local `docker build .`. Actions use full version
tags and every step has a name. A develop push publishes the image as `latest`;
release tags publish versioned images. Do not push, merge or release implicitly.

Use focused Go tests first, then applicable CI checks. `internal/web` race tests
can take about five minutes; use `adminstest.Hash` for cheap synthetic credentials.
`make fixtures` regenerates synthetic XLSX files. `make diagrams` uses local
Node/npx to regenerate committed `kb/<id>.svg` files and source hashes; `kb.Load`
rejects stale diagrams. CSS is generated by `make css`; do not edit `app.css`.
For JavaScript changes, run `node --check internal/web/static/app.js` when available.
Browser-check changed UI and forms: tests with hand-written Origin headers do not
prove browser behavior. Simplify the diff, then use Ponytail review if available;
otherwise review manually and report the fallback. Report unrun checks explicitly.

## Implementation Pitfalls

### SQLite and Go

Never edit a shipped migration: migration tracking uses numbers only. Add
`NNNN_description.sql` with the next unused number. Externally visible SQLite IDs
need `AUTOINCREMENT` to prevent reuse. Changing a `CHECK` requires rebuilding the
table and its referencing tables, filling replacements before dropping originals;
see `0006_mollie.sql`. Backups use rollback-journal mode so they open read-only.
Preserve response-body close errors; use the existing named-error-return pattern
in `suggest`, `turnstile` and `push`.

Use escaped invisible Unicode characters in Go tests and verify the bytes after
editing: tools can normalize them. `html/template` escapes `+` and apostrophes;
nest `if` inside `with` rather than using `else if` after `with`. A map indexed by
template string literals needs string keys. Preserve explicit embedding of
`templates/_*.html`: directory embeds skip underscore/dot-prefixed files.

### CSS, Forms and PWA

Tailwind scans templates plus the explicit inline source in
`internal/web/css/input.css`; JavaScript toggles attributes (`hidden`, `data-*`),
not classes. `.panel` belongs in `@layer components` so utilities can override it.
daisyUI control height is `--size-field` times ten; preserve 44px touch targets.
Never disable the submitter before submission: its `name=value` would disappear.

iOS installed apps avoid `viewport-fit=cover` and safe-area padding; form controls
must stay at least 16px to prevent Safari zoom. Push subscription must begin on a
direct tap, before asynchronous work, and each push must show a notification.
`VAPID_SUBJECT` is a bare `mailto:club@example.org`, never angle-bracketed.
iOS reuses the installed app window: retain the service-worker `{open: url}`
message and the unsaved-form guard in `app.js`. Cache names hash embedded assets;
preserve the update banner on changes.

Playwright routes do not intercept service-worker fetches; block service workers
when testing network failures. Headless Chromium cannot register real Web Push;
use CDP delivery to test the handler and inspect notifications. Installability
checks need a persistent profile. Browser acceptance must respect public-form
rate limits; use multiple synthetic addresses instead of weakening the limits.

### Mail and Telemetry

SMTP rejects plaintext and untrusted certificates; end-to-end checks need Mailpit
TLS and its CA through `SSL_CERT_FILE`. Umami records page views only; use a regular
browser user agent for checks because it excludes HeadlessChrome. Sentry is fully
disabled in development; invalid Sentry/Umami settings warn and disable the service.
Error-level logs become Sentry events; expected refusals belong at Warn or below.
Preserve the explicit Sentry exporter endpoint instead of inheriting unrelated
`OTEL_EXPORTER_OTLP_*` settings; custom test transports need a flush.
