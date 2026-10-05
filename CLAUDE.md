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
- No container image publication: CI builds the image only (§9.4).
- `sessions.credential_hash` (§8.2): a session is valid only while it matches
  the account's current password hash.
- `.env.example` lists only the variables the binary reads; each lot adds its own.

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
- Every new dependency is justified in the README.

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

## Layout and commands

- `cmd/sos-vpdive` (subcommands) + `internal/{config,secure,telemetry,store,xlsx,members,admins,web}`.
  Migrations: `internal/store/migrations/NNNN_*.sql`. Content files `config/*.yaml`
  are embedded by the root `content.go`.
- `make test` / `make lint` (golangci-lint v2, `default: all`) / `make css` /
  `make build` / `make fixtures` (regenerates `testdata/fixtures/*.xlsx`).
- `make run` needs `.env` (from `.env.example`, `APP_ENV=development`) and
  `admins/admins.yaml`; sites on `http://sos.localhost:8080` and
  `http://comite.localhost:8080` (browsers treat `*.localhost` as secure).
- `./scripts/check-forbidden-files.sh` is the CI guard on private files.

## Workflow

- Gitflow, `feature/*` from `develop`. Greenfield: no PR, merge `--no-ff` into
  `develop` and push once everything is green; `main` is a release decision.
- Before merging a lot: `/simplify`, `/ponytail:ponytail-review`, `/codex:review`,
  and a browser check (`playwright-cli`) of every page with a form.
- Plans and designs go in `docs/superpowers/` (gitignored, never committed).

## Gotchas

- Tests that set `Origin` by hand cannot catch browser behaviour: verify forms
  in a real browser.
- Write invisible characters in Go tests as escapes (`\u00a0`, `\u0301`):
  editors and agents normalise raw ones away.
- html/template outputs `+` as `&#43;` and `'` as `&#39;`.
- `internal/web` tests take 30–60 s (argon2id at 64 MiB per login).
- daisyUI control height is `--size-field` × 10: the theme sets 0.275rem for 44 px.
- Mount the accounts file's directory, never the single file: a file bind mount
  pins the inode and hot reload never sees rename-saves.
- Backups are switched to a rollback journal: a WAL-flagged copy cannot be opened
  read-only.
- The Dockerfile's CSS stage downloads Tailwind from GitHub; the CI `image` job
  is its end-to-end check.
