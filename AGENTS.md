# Dear Agent

maragudb is an embedded Go SQL database whose only durable state is in S3-compatible object storage.

- Requirements: docs/requirements.md
- Decisions: docs/decisions.md
- Implementation diary: docs/diary/

Nobody reviews this code manually. Automated tests are the only quality gate, so tests must be strong enough to catch real bugs.

## Commands

- `make test` runs the full test suite, including the fixed simulator seed corpus.
- `make soak` runs the longer simulator soak.
- `make replay SEED=<n>` replays one simulator seed.
- `make verification` regenerates the mutant kill matrix and /docs/verification.md, which must then show no diff.
- `make report` renders the verification report of the last `make test` and `make antithesis`.
- `make pgdiff PGDIFF_CASES=<n>` runs the Postgres differential harness with n cases.
- `make ratchet` rewrites the baseline of statements that the differential harness executes on both sides.
- `make lint` runs golangci-lint.
- `make fmt` formats the code.
