# maragudb

> [!WARNING]
> **This is an experiment, not production software. Do not trust it with data you care about.**
>
> The experiment asks: can you build an S3-backed SQL database you can trust when no human ever reads the code? An AI writes the code, and the author, Markus, deliberately never looks at it. Trust has to come from automated verification alone: deterministic simulation testing with fault injection, strict serializability history checkers, mutation tests that prove the checkers catch deliberate bugs, property-based tests, differential testing against Postgres, and integration tests against real S3-compatible storage (Tigris).
>
> APIs, the storage format, and everything else may change without notice.

[![Docs](https://pkg.go.dev/badge/maragu.dev/maragudb)](https://pkg.go.dev/maragu.dev/maragudb)
[![CI](https://github.com/maragudk/maragudb/actions/workflows/ci.yml/badge.svg)](https://github.com/maragudk/maragudb/actions/workflows/ci.yml)

An embedded Go SQL database whose only durable state is in S3-compatible object storage.

Made with ✨sparkles✨ by [maragu](https://www.maragu.dev/): independent software consulting for cloud-native Go apps & AI engineering.

[Contact me at markus@maragu.dk](mailto:markus@maragu.dk) for consulting work, or perhaps an invoice to support this project?
