# arxiv CLI

This reference is derived from `cmd/arxiv/main.go` and `cmd/arxiv/serve.go`.

## Build

```bash
go build -o bin/arxiv ./cmd/arxiv
./bin/arxiv help
```

`ARXIV_CACHE` selects the file cache root and defaults to `~/.cache/arxiv`. `DATABASE_URL` selects PostgreSQL when non-empty; an empty value selects the legacy SQLite database at `$ARXIV_CACHE/index.db`. Use PostgreSQL with pgvector for supported deployments.

## Commands

### `fetch`

Fetch metadata and files for one or more paper IDs:

```text
arxiv fetch [-pdf] [-source=true] [-all] [-with-embedding] <paper-id> [paper-id...]
```

- `-source` downloads TeX source and defaults to `true`.
- `-pdf` downloads the PDF and extracts text.
- `-all` downloads source and PDF.
- `-with-embedding` queues the canonical Qwen abstract and full-paper preparation jobs.

### `sync`

Sync metadata through arXiv OAI-PMH:

```text
arxiv sync [-set cs] [-from YYYY-MM-DD] [-batch 1000]
```

This syncs metadata, not every PDF or source archive. Interruptions resume from durable sync state.

### `stats`

```text
arxiv stats
```

Print paper, PDF, source, and queued-download counts.

### `search`

```text
arxiv search [-category cs.AI] [-limit 20] "query"
```

Runs database full-text search across cached metadata. Qwen idea/deep search is exposed through the web/API interfaces, not this CLI command.

### `get`

```text
arxiv get [-fetch] <paper-id>
```

Reads one cached paper. `-fetch` retrieves missing metadata from arXiv.

### `list` / `ls`

```text
arxiv ls [-cat cs.AI] [-n 50] [-src] [-a] [category]
```

- `-src` requires downloaded source.
- `-a` includes metadata-only papers.
- `-n 0` means no explicit result limit.

### `reindex`

```text
arxiv reindex
```

Rebuilds the full-text index and citation data. The former `-embeddings` MiniLM path is retired and returns an error; use the Qwen pipeline in [SEMANTIC_SEARCH.md](SEMANTIC_SEARCH.md) for current semantic data.

### `serve`

```text
arxiv serve [-port 8080] [-local]
```

- `-local` enables local PDF/source caching, bypasses production privileged checks, and binds to loopback.
- MiniLM service and worker flags have been removed. Qwen workers run through the documented worker pipeline.
- `QWEN_EMBEDDING_SERVICE_URL` configures a synchronous Qwen service when available; otherwise Qwen query/abstract work can be queued for remote workers.

Normal serving listens on all interfaces. Put it behind a trusted ingress and configure authentication before exposing it.

## Migration Command

`cmd/migrate` is a separate SQLite-to-PostgreSQL utility:

```bash
go run ./cmd/migrate \
  -sqlite /path/to/index.db \
  -postgres "$DATABASE_URL" \
  -batch 500
```

The PostgreSQL URL is required through `-postgres` or `DATABASE_URL`. Rehearse against a disposable database and back up the target before use.
