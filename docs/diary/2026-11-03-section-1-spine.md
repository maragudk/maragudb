# Diary: maragudb section 1 (Spine)

This diary covers section 1 of maragudb, an embedded Go SQL database whose only durable state is in S3. Section 1 builds the storage engine, optimistic multi-writer commits, compaction and GC, a minimal SQL layer, the `database/sql` driver, and the verification infrastructure:

- a fault-injecting fake S3
- a deterministic simulator with serializability checkers
- a Postgres differential harness
- a Tigris integration suite
- an Antithesis harness

The full requirements are in /docs/requirements.md, and the project-level decisions are in /docs/decisions.md.

## Step 1: Requirements interview and project setup

**Author:** main

### Prompt Context

**Verbatim prompt:** I'd like to build a new database with SQL query syntax but backed by S3 natively, no local storage. Similar in spirit to SQLite but without its quirks. Rethought from the ground up to work cloud-native. Interview me about requirements (don't use the brainstorm skill) and let's figure out a verifiable goal you can build against. I won't be reviewing any code in this project manually, we'll rely on automated testing (maybe that queries with the same data need to return the same results as for SQLite or something where applicable) and perhaps property-based testing (see your hegel skill).
**Interpretation:** Interview Markus, turn the answers into concrete requirements and automated acceptance criteria, and then delegate the building.
**Inferred intent:** Get a database whose correctness is proven by machines, because no human will read the code.

### What I did

I interviewed Markus over four rounds of questions. Then I ran `git init` on `main` in an empty directory and wrote /docs/requirements.md and /docs/decisions.md. I split section 1 so that the storage and transaction core, together with its simulator and checkers, comes first and the SQL layer comes after it.

### Why

The highest risk is ACID on S3 with several writers. If that core is wrong, nothing built on top of it matters, so it must be proven before the SQL surface grows.

### What worked

The answers converged quickly:

- an embedded library with explicit `RegisterDriver`
- multi-writer and strict serializable
- turbopuffer-style caches
- Postgres as the oracle, with an AST rendered to both dialects
- Go-like types
- request-count budgets as the performance gates
- Tigris for integration tests

### What didn't work

Nothing failed. The first version of the Antithesis question was rejected so that Markus could install the antithesis-skills plugin first. After that, he chose "Local DST + Antithesis-ready".

### What I learned

- Tigris supports conditional PUTs and returns 412 on a mismatch. The header `X-Tigris-Consistent: true` routes a request to the leader.
- The Antithesis Go SDK instrumentation is amd64-only, but Go cross-compiles, so this Mac does not block the Antithesis harness.

### What was tricky

The two requirements "without SQLite's quirks" and "same results as SQLite" contradicted each other. The fix was to make Postgres the oracle, compared through a shared query AST, with intentional deviations documented.

### What warrants review

Check the acceptance criteria in /docs/requirements.md. They are the only quality gate.

### Future work

- Add the GitHub remote after Markus creates the repo.
- Spawn the builder for the storage and transaction core.
