# askdocs

RAG over any folder of docs — **one binary, one SQLite file, nothing else deployed**.

Point it at a directory of markdown/text. It chunks the files, embeds them, and
stores chunks + full-text index + vector index side by side in a single
SQLite file (FTS5 + [sqlite-vec](https://github.com/asg017/sqlite-vec)). Then
search it — keyword, semantic, or fused — and ask questions answered by an LLM
with citations, streamed token-by-token into an HTMX web UI over SSE.

Built to demonstrate the Go + HTMX + SQLite thesis: the entire retrieval
layer of a RAG app collapses into a file. No vector database, no Elasticsearch,
no Python, no docker-compose.

## Install

```sh
go install github.com/valkyraycho/askdocs@latest
```

Pure Go (`CGO_ENABLED=0`) — SQLite and sqlite-vec run as WASM via
[ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3). Cross-compiles
to macOS/Linux/Windows with plain `GOOS=... go build`.

## Configure a provider

Any OpenAI-compatible endpoint works:

```sh
# OpenAI
export OPENAI_API_KEY=sk-...

# an internal gateway
export OPENAI_BASE_URL=https://your-gateway.example.com/v1
export OPENAI_API_KEY=...

# fully local via Ollama (no key needed)
export OPENAI_BASE_URL=http://localhost:11434/v1
export ASKDOCS_EMBED_MODEL=nomic-embed-text
export ASKDOCS_CHAT_MODEL=llama3.1
```

Defaults: `text-embedding-3-small` for embeddings, `gpt-4o-mini` for answers.
Plain-http endpoints are rejected unless loopback (`ASKDOCS_ALLOW_INSECURE=1`
to override).

## Use

```sh
askdocs ingest ./docs               # builds ./docs/askdocs.db
cd docs
askdocs search sqlite busy timeout  # hybrid keyword+semantic search
askdocs ask "how do we configure retries?"
askdocs web                         # http://127.0.0.1:4712 — live search + streamed answers
askdocs status
```

Every corpus is one `.db` file living next to the docs it indexes. Another
folder? Ingest it — it gets its own file. Deleting a knowledge base is
deleting a file.

Re-running `ingest` is incremental: files are hashed, unchanged files cost
nothing, changed files are re-embedded, deleted files are pruned.

Files whose names start with the corpus database's name (e.g.
`askdocs.db-anything.md`) are skipped by the walker along with the database
itself and its WAL sidecars; symlinks are never followed.

## Honest notes

- **Your documents leave the machine at ingest time** — chunks are sent to
  the embedding endpoint you configured (questions are sent at ask time).
  The ingest command prints exactly where before it starts. For an air-gapped
  setup, use Ollama.
- **Embeddings are corpus-fixed.** Switching embedding models means
  re-ingesting into a fresh db (the corpus stamps its model/dims/provider and
  refuses mismatches).
- **WAL sidecars:** while a process has the corpus open you may see
  `askdocs.db-wal`/`-shm` next to it. A cleanly finished `ingest`
  checkpoints them away; copy or delete the full `askdocs.db*` set.
- **`askdocs web` is localhost-only** (binds 127.0.0.1, validates Host,
  rejects cross-site requests via Sec-Fetch-Site, strict CSP). It is not an
  internet-facing service and does no auth — shared multi-user machines
  should assume other local users can reach the port.
- **Version pins:** sqlite-vec support rides on
  `sqlite-vec-go-bindings@v0.1.6`, which requires `ncruces/go-sqlite3@v0.21.x`
  and the wazero threads feature (enabled at init). Don't bump the driver
  without re-running the spike test (`go test ./internal/store -run TestSpike`).
- **wazero must stay ≥ v1.11**: the sqlite-vec WASM uses atomics, and
  wazero v1.8's experimental threads implementation crashed intermittently
  (`out of bounds memory access`) under load. All three OSes are
  CI-verified on the pinned combination.

## Development

```sh
go test ./...    # hermetic — a fake OpenAI-compatible server, no API calls
```

## License

MIT
