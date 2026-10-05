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
