# Contributing to arxiv.gg

Thanks for contributing. Keep changes focused, explain user-visible behavior, and avoid presenting experimental or legacy paths as supported production features.

## Prerequisites

- Go 1.25, matching `go.mod`
- PostgreSQL with pgvector for production-path development
- Python 3 and a CUDA-capable environment when changing Qwen workers
- `pdftotext` from Poppler when changing PDF ingestion
- SQLite only for the existing legacy/unit-test path

## Setup

```bash
git clone https://github.com/lantos1618/arxiv.gg.git
cd arxiv.gg
go mod download
go test ./...
go build -o bin/arxiv ./cmd/arxiv
```

Use an explicit PostgreSQL URL for application and integration work:

```bash
export ARXIV_CACHE="$PWD/.cache/arxiv"
export DATABASE_URL='postgres://arxiv:password@127.0.0.1:5432/arxiv?sslmode=disable'
./bin/arxiv serve -port 8080
```

Clearing `DATABASE_URL` selects SQLite. Do that only for tests or deliberate legacy-backend checks; do not infer production correctness from a SQLite pass.

## Development Rules

- Run `gofmt` on changed Go files and follow existing package boundaries.
- Keep API errors safe for public clients; log operational detail server-side.
- Preserve request cancellation and bound public request size, limits, and fan-out.
- Treat queue claims, leases, completion, retries, and database/cache transitions as concurrency-sensitive.
- Use Qwen (`Qwen/Qwen3-Embedding-8B`, 1,024 dimensions) for current semantic features. MiniLM files and the original embedding table are compatibility code, not a second supported architecture.
- Never put secrets in source, examples, command arguments, logs, or committed `.env` files.
- Do not add claims about coverage, performance, plans, pricing, or awards without current evidence and precise conditions.

## Tests

Start narrow, then run the full checks:

```bash
go test ./... -run TestName
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

Changes to PostgreSQL SQL, semantic search, OAuth, proxy trust, deployment, or workers also need focused integration validation. See [docs/TESTING.md](docs/TESTING.md).

## Pull Requests

Include:

1. The problem and intended behavior.
2. The files and interfaces changed.
3. Tests and manual validation performed.
4. PostgreSQL, migration, security, privacy, and deployment impact.
5. Any follow-up work that remains genuinely out of scope.

Avoid unrelated formatting or refactors. Do not commit generated binaries, runtime logs, local databases, model caches, credentials, or `.env` files.

## Documentation

Update the active docs when commands, routes, environment variables, account behavior, Qwen architecture, or deployment steps change. Date-stamped audits and reports are archived evidence: add a current disposition instead of silently rewriting historical observations as if they were new measurements.

Contributions are licensed under the repository's MIT license.
