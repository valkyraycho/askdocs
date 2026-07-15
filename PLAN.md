# Plan: `askdocs` — RAG over any folder, one binary + one SQLite file
_Locked via grill — by Claude + Ray Cho. Revised after Codex review round 1._

## Goal

A local-first POC that demonstrates the Go + HTMX + SQLite thesis with the stack's distinctive claims load-bearing: point the binary at a folder of docs, and it builds **one SQLite file** containing the chunked text, an FTS5 index, and vector embeddings (sqlite-vec) side by side — the entire RAG retrieval layer with nothing else deployed. A concurrent Go pipeline ingests (walk → chunk → embed → write); an HTMX web UI provides FTS live search, hybrid (RRF) search, and LLM answers streamed token-by-token over SSE with citations. One corpus = one `.db` file that lives next to the docs. Embeddings/chat via any OpenAI-compatible endpoint (RDSEC gateway, OpenAI, Ollama). Single static binary, `CGO_ENABLED=0`, published to `github.com/valkyraycho/askdocs` (MIT).

## Approach

1. **Scaffold**: repo at `~/go-htmx-sqlite/askdocs`, `go mod init github.com/valkyraycho/askdocs`, MIT license, `.gitignore` (incl. `askdocs.db*`). Layout:
   ```
   main.go                  # stdlib subcommand dispatcher: ingest, search, ask, web, status
   internal/store/          # SQLite via ncruces/go-sqlite3 + sqlite-vec: schema, chunks, FTS, KNN, RRF
   internal/ingest/         # walker, markdown-aware chunker, concurrent embed pipeline
   internal/llm/            # OpenAI-compatible client (embeddings + streaming chat), net/http only
   internal/web/            # HTMX UI: live search, hybrid search, SSE ask panel (patterns ported from til)
   ```
2. **Store layer** — driver `github.com/ncruces/go-sqlite3` (WASM/wazero, CGO-free) + `github.com/asg017/sqlite-vec-go-bindings/ncruces` (importing the binding swaps in the sqlite-vec-enabled WASM build). **Startup smoke test on every open: `SELECT vec_version()`** — a broken extension contract fails loudly, not at first query. **The very first commit is a blocking spike**: a store test that creates a `vec0` table, inserts known vectors, and asserts KNN order — pushed immediately so the 3-OS CI matrix proves sqlite-vec works everywhere *before* anything is built on it; if the spike fails on any platform, the plan comes back for explicit revision (the BLOB+Go-cosine alternative is a different schema and a weaker product claim, not a drop-in).
   - **Only `ingest` may create a database — split open paths.** `OpenIngest` (writable) runs migrations inside `BEGIN IMMEDIATE`; `OpenReadOnly` (used by search/ask/status/web) `os.Stat`s first (missing → "no corpus at <path> — run askdocs ingest"), opens with the read-only flag, and only *verifies* `application_id` + `user_version` — it never migrates. `PRAGMA application_id` is stamped (`0x41534B44`, "ASKD") at creation; an empty or foreign SQLite file is a clear error. `wal_checkpoint(TRUNCATE)` runs only on writable (ingest) close, best-effort on BUSY.
   Schema, created in **two phases**:
   - Phase 1 (every `OpenIngest`, `user_version=1`, inside `BEGIN IMMEDIATE` with re-read + SQLITE_BUSY retry — til's hardening; `OpenReadOnly` never migrates):
   ```sql
   CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);  -- schema_version, corpus_root, embed_model, embed_dims, embed_provider_host
   CREATE TABLE files (
     path  TEXT PRIMARY KEY,              -- relative to corpus_root
     hash  TEXT NOT NULL,                 -- sha256 of the exact bytes that were chunked
     ingested_at TEXT NOT NULL
   );
   CREATE TABLE chunks (
     id      INTEGER PRIMARY KEY AUTOINCREMENT,
     path    TEXT NOT NULL REFERENCES files(path) ON DELETE CASCADE,
     heading TEXT NOT NULL,               -- breadcrumb: "README.md › Install › Docker"
     content TEXT NOT NULL,
     pos     INTEGER NOT NULL
   );
   CREATE INDEX idx_chunks_path ON chunks(path);
   CREATE VIRTUAL TABLE chunks_fts USING fts5(heading, content, tokenize='porter unicode61');
   ```
   - Phase 2 (**lazy, at first successful embedding batch** — resolves the dims-before-first-response circularity): `CREATE VIRTUAL TABLE chunks_vec USING vec0(embedding float[<dims>])` + stamp `embed_model`/`embed_dims`/`embed_provider_host` into meta, one transaction. `hybrid`/`ask` on a corpus without phase 2 → clean error: "corpus has no embeddings yet — run askdocs ingest".
   - **Embedding-space identity enforced**: model or dims mismatch is a hard error with **no override** — the only path is re-ingesting into a fresh db (mixing spaces is corruption, not migration). Provider-host mismatch is also a hard error, but `ASKDOCS_ALLOW_PROVIDER_MISMATCH=1` may assert "same embedding space behind a different URL" (e.g. gateway migration) — model and dims must still match exactly. **Restamping happens only through an override-enabled `ingest`** (read-only commands cannot write meta); until that ingest runs, search/ask/web keep hard-erroring.
   - All FTS/vec writes address rowid = `chunks.id` explicitly, same transaction as the chunk row (til lesson).
   - **FTS query builder ported from til**: user input never reaches `MATCH` raw — whitespace-tokenize, quote each token (`""`-escape), prefix-star the last, 256-char cap, blank input short-circuits without MATCH.
   - Search API: `SearchFTS(q, limit)`, `SearchVec(embedding, limit)`, `SearchHybrid` = RRF `score = Σ 1/(60+rank)` over both lists. `limit` clamped to 1..50; candidate pool per list = `max(20, 4×limit)` capped at 100; **deterministic tie-break**: fused score desc, then chunk id asc (slices, not map iteration).
   - Corpus DB default for `ingest`: **`<corpus-root>/askdocs.db`** (beside the docs it indexes); other commands default `./askdocs.db`; `--db` flag / `ASKDOCS_DB` override everywhere. `corpus_root` stored canonical-absolute at first ingest; a later ingest with a different root hard-errors (prevents cross-corpus path collisions and mass pruning). The walker skips the corpus db file and its `-wal`/`-shm` sidecars. File 0600; WAL sidecar backup caveats documented as in til.
3. **LLM client** (`internal/llm`) — plain `net/http`, no SDK:
   - Config from env: `OPENAI_BASE_URL` (default `https://api.openai.com/v1`; taken verbatim including its path — the client appends only `/embeddings` and `/chat/completions`, so Ollama is exactly `http://localhost:11434/v1`), `OPENAI_API_KEY` (**optional when the base host is loopback** — Ollama needs no key; required otherwise, validated at startup of ingest/ask/web, never logged; `Authorization` header sent only when set), `ASKDOCS_EMBED_MODEL` (default `text-embedding-3-small`), `ASKDOCS_CHAT_MODEL` (default `gpt-4o-mini`). **Plain-HTTP endpoints rejected unless loopback** (`http://` to a remote host would leak the key and document contents; `ASKDOCS_ALLOW_INSECURE=1` is the explicit override).
   - `Embed(ctx, []string) ([][]float32, error)` — batched (≤32/request). **Retries bounded**: ≤3 attempts, exponential backoff with jitter, `Retry-After` honored, context-aware, response bodies capped (10MB) — four workers cannot amplify an outage into a retry storm. **Responses strictly validated**: items reordered by `index`; wrong count, duplicate/missing indices, dimension mismatch, or non-finite floats → error (never silently misaligned vectors).
   - `ChatStream(ctx, prompt) (<-chan string, <-chan error)` — parses the OpenAI SSE stream; context cancellation aborts the upstream request. Non-streaming fallback (`stream:false`) behind a flag if the gateway's SSE proxying misbehaves.
4. **Ingest pipeline** (`internal/ingest`):
   - Walker: includes `.md .markdown .txt .rst`; skips hidden directories **and hidden files**, `node_modules`, `vendor`, `dist`, files >1MB, and the corpus db + sidecars.
   - **Egress visibility**: before any embedding call, print the provider host and counts — `embedding 42 files (1,204 chunks) via api.rdsec.trendmicro.com` — so "local-first" never silently means "documents leave the machine". Non-interactive by design (a printed notice, not a prompt); README documents the egress plainly.
   - **Read once, use once**: each file is read into memory a single time (≤1MB bound); sha256 and chunks both derive from those exact bytes — hash and content can never disagree.
   - **Incremental, per-file atomic**: unchanged hash → skip (no API cost). Changed file → chunk + embed **completely**, then one transaction replaces its files/chunks/FTS/vec rows; an embedding failure mid-file leaves the previous version fully intact.
   - **Prune only after a complete, error-free walk**: deletions are computed from a finished manifest; any walk error or cancellation skips the prune phase entirely.
   - **Single-ingest lock**: `<db>.lock` created with `O_CREATE|O_EXCL` (pid inside) for the whole scan→commit lifecycle; a concurrent ingest fails fast with "another ingest is running (remove <db>.lock if stale)".
   - **Chunker**: markdown-heading-aware — sections per heading, breadcrumb (`path › H1 › H2`) prepended to embedded text and stored in `heading`; sections >2000 chars split on paragraph boundaries with one-paragraph overlap (overlap itself capped at 500 chars); **a single paragraph/code block exceeding the budget is hard-split rune-safely at 2000 chars** so no chunk can approach the 1MB file bound or embedding-input limits; `.txt/.rst` fall back to paragraph packing. No tokenizer dependency.
   - **Concurrency**: walker → chunk channel → 4 embed workers (batched calls) → single DB-writer goroutine (file-complete transactions). Progress to stderr: `42/128 files · 1,204 chunks · 3 skipped (unchanged)`.
   - **Failure observability**: every failure emits one bounded stderr line — stage (walk/chunk/embed/write), file path, attempt, HTTP status, latency — never document content or credentials. A summary of failed files prints at the end (and, per #7, any failure suppresses pruning).
5. **CLI** (stdlib dispatcher, patterns from til). **Flags come before positional arguments** — that's how Go's `flag.FlagSet` parses, and the help text/README show it that way consistently:
   - `askdocs ingest [-db f] <path>` — build/update the corpus (the only command that may create the db).
   - `askdocs search [-db f] [-n 10] <query>` — hybrid in the terminal (FTS-only with a notice when no API key).
   - `askdocs ask [-db f] <question>` — retrieval + streamed answer to stdout, `[n]` citations listed after.
   - `askdocs status [-db f]` — files, chunks, embed model/dims/provider, db + WAL sidecar sizes.
   - `askdocs web [-db f] [-port 4712]` — serve on 127.0.0.1.
   - Only writable (ingest) store close runs `PRAGMA wal_checkpoint(TRUNCATE)` (best-effort on BUSY) so a cleanly-closed corpus is genuinely one file; read-only closes never checkpoint. Docs qualify the "one file" claim with the WAL sidecar reality (copy/delete the full `askdocs.db*` set).
6. **Web UI** (`internal/web`) — hardening ported from til (bind 127.0.0.1, Host validation, CSP `default-src 'self'`, nosniff/no-referrer/no-store, listen-before-print, graceful shutdown) **plus new guards for costly endpoints**:
   - **`Sec-Fetch-Site` middleware on every request**: values other than `same-origin`/`none` (or absent — curl/EventSource same-origin sends it) → 403. This closes the cross-site-GET hole (`<img src=…/ask/stream?q=…>` triggering API spend) without capability tokens or breaking EventSource, which can't do POST.
   - **Ask concurrency + input bounds**: a semaphore caps concurrent `/ask/stream` at 2; `q` capped at 512 chars; both return friendly errors.
   - **SSE write safety instead of a global `WriteTimeout=0`**: `http.ResponseController.SetWriteDeadline` before every SSE write (per-write deadline ~10s — a stalled client is dropped), stream lifetime capped by request context (10 min). Header/read timeouts unchanged.
   - **Reconnect-billing loop prevented, including mid-stream failures**: (a) every terminal outcome — success *or upstream error* — sends a final event whose fragment **replaces the element holding `sse-connect`** (success: rendered answer; failure: an error panel with a retry button), removing the EventSource so htmx cannot auto-reconnect; (b) for the case the server can't send anything (network drop), the connector URL carries a **one-use nonce** — the server keeps a small in-memory TTL set (10 min, bounded) and an EventSource auto-reconnect replaying a seen nonce gets an immediate terminal "connection lost — ask again" event with **no LLM call**. A real reconnect is exercised in tests.
   - **Standards-compliant SSE framing, centralized**: one `sseWrite(w, event, data)` encoder splits data on newlines into per-line `data:` fields (tokens and HTML fragments routinely contain `\n`; naive framing truncates events or lets payload lines masquerade as `event:` fields). Tested with multiline data, blank lines, and literal `event:`/`data:` text inside payloads.
   - **Answer output bounded**: chat requests set `max_tokens` (default 1024, `ASKDOCS_MAX_ANSWER_TOKENS` override); the accumulating buffer for the final rendered fragment is capped (256KB) — exceeding it cancels upstream and terminates the stream with the error panel.
   - **XSS discipline for streamed content**: every `token` event's text is HTML-escaped server-side before framing; `citations` and `done` fragments come from `html/template`; markdown rendered by goldmark with raw HTML disabled (til's render package pattern). FTS snippets are template-escaped like everything else.
   - **Citations validated, context treated as data**: retrieved chunks are wrapped in explicit delimiters with a system-prompt line stating the content is reference data, not instructions (cheap prompt-injection hygiene; residual risk documented). Rendered `[n]` citations are validated against the actual source set — an out-of-range citation renders as plain text, never a dead link.
   - Endpoints: `GET /` (corpus header, search + ask boxes, files overview) · `GET /search?q=` keystroke tier, FTS-only, labeled `keyword` · `GET /hybrid?q=` submit tier, embeds query once, labeled `hybrid` · `GET /chunks/{id}` chunk detail (rendered markdown, path, neighbors) · **`GET /ask/connect?q=`** — the ask form targets this; it mints a fresh nonce, registers it in the TTL set, and returns the connector fragment (`hx-ext="sse" sse-connect="/ask/stream?q=…&once=<nonce>"` plus the empty citations/answer slots) · `GET /ask/stream?q=&once=` SSE: `citations` event → `token` events → terminal event (answer or error panel) replacing the connector.
   - Templates + htmx.min.js + the SSE extension embedded via `go:embed`; terminal-dark aesthetic as a later polish pass.
7. **Tests** (TDD, table-driven, ≥80%):
   - **Fake OpenAI server** (`httptest`): `/embeddings` returns deterministic hash-derived vectors — *deliberately out of order with index fields set* to exercise reordering; `/chat/completions` streams a canned SSE answer. Zero real API calls.
   - store: two-phase migration (+ busy retry, idempotency), vec_version smoke test, chunk CRUD cascade, FTS builder hostile inputs, vec KNN roundtrip with known vectors, RRF fusion table cases + deterministic tie-break, model/dims/provider mismatch rejection, no-embeddings-yet error.
   - ingest: chunker golden cases; incremental (edit 1 of 3 files → only it re-embeds); **mid-file embedding failure leaves old version intact**; **walk error skips prune**; lockfile conflict; hidden-file and db-file skipping; read-once hash/content agreement.
   - llm: batching, 429 retry, response validation (reorder, bad dims, NaN), stream parse, context cancel.
   - web: search/hybrid fragments, security headers, Host check, Sec-Fetch-Site 403 vs same-origin/absent allow, ask semaphore limit, oversized q rejected. **SSE tested through a real `httptest.Server`** (not a ResponseRecorder): assert the first event arrives *before* the handler completes (proves flushing/streaming), then drop the client connection and assert the fake chat upstream observes context cancellation. **Reconnect billing test**: replay the same nonce → terminal event, zero upstream calls. **SSE encoder cases**: multiline/blank-line/`event:`-text payloads. Upstream-failure path sends the error-panel terminal event. Answer-cap exceeded cancels upstream. Citation validation cases (out-of-range `[n]` inert).
   - llm keyless mode: fake server without auth verifies no `Authorization` header is sent and loopback-keyless startup validation passes.
   - CI: same 3-OS matrix as til (`CGO_ENABLED=0`) — validates ncruces/wazero + sqlite-vec on windows.
8. **Publish**: same flow as til (gh account switch to valkyraycho, username-qualified HTTPS remote to dodge the gitconfig SSH rewrite).

## Key decisions & tradeoffs

- **sqlite-vec via ncruces (WASM) over modernc + Go cosine** — the thesis demands "vector search *inside* SQLite"; CGO stays 0. Costs: younger bindings, ~8MB wazero weight, new driver API. **De-risked by a day-one blocking spike through the CI matrix**; if it fails anywhere, the plan returns for explicit revision — the BLOB+Go-cosine alternative is acknowledged as a schema change and a weaker claim, not a silent drop-in.
- **Sec-Fetch-Site check over POST+capability-token for costly GETs** — EventSource can't POST; a token dance adds state the POC doesn't need. Sec-Fetch-Site is one middleware, ships in every modern browser, and non-browser clients (curl) don't send it and pass. Residual: pre-2020 browsers — acceptable for a localhost dev tool.
- **Terminal-event-replaces-connector plus one-use nonce** for SSE reconnect billing — removing the EventSource element is the htmx-native way to end a stream (covers success and upstream errors); the nonce TTL set (bounded, in-memory, 10 min) covers the network-drop case where the server can't send a terminal event. This is deliberately the only server-side state in the app, and it's ephemeral.
- **Printed egress notice over interactive confirmation** — ingest stays scriptable; the notice + README keep it honest. Anyone needing an air gap uses Ollama.
- **Lockfile (`O_EXCL`) over DB-level ingest lock** — the race spans scan/embed phases where no DB transaction lives; a pid-stamped lockfile is 10 lines and names the fix when stale.
- **Two-tier search: keystroke = FTS-only, submit = hybrid** — embedding per keystroke is ~300ms + money; the split also makes semantic recall visibly better on submit.
- **RRF over score normalization** — bm25 and cosine are incomparable; fuse ranks.
- **SSE ask holds no per-request state beyond the nonce set** — retrieval happens inside the stream handler; an auto-reconnect replaying a nonce terminates instantly with no model call; a *deliberate* retry goes back through `/ask/connect` and is a new ask by design.
- **`<corpus-root>/askdocs.db` for ingest** — the corpus file provably lives beside the docs regardless of cwd; enforced `corpus_root` prevents cross-corpus reuse.
- **Provider-host stamped with model+dims** — same model name behind a different endpoint won't silently mix embedding spaces. The env override is strictly an *equivalence assertion* (same space, new URL), applied only via an override-enabled ingest; model/dims mismatches have no override — a fresh re-ingest is the only path.
- **OPENAI_* env convention**; **read-only web UI in v1** (no mutation endpoints; til's Origin-check pattern on the shelf); **no tokenizer dependency**; **dependencies capped at three** (ncruces driver, sqlite-vec bindings, goldmark).

## Risks / open questions

- **sqlite-vec + ncruces on all three CI OSes** — resolved by the blocking spike (first commit) rather than assumed; failure = explicit plan revision.
  - *Spike outcome*: bindings v0.1.6 require ncruces **v0.21.x** (not the v0.17.1 their go.mod declares, and not ≥v0.33 which drops `sqlite3.Binary`) plus the wazero **threads** core feature enabled via a custom `RuntimeConfig`. Pinned: ncruces v0.21.3 + wazero v1.8.2 (v0.21.3's minimum — no downgrade room). macOS + Linux pass; **Windows fails inside the sqlite-vec wasm** (`out of bounds memory access`, upstream bug with no pin escape). Scope adjusted per this contract: Windows CI is advisory (`continue-on-error`), limitation documented in README; revisit when the bindings ship a fixed wasm.
- **RDSEC gateway streaming quirks** — non-streamed fallback flag if SSE proxying misbehaves.
- **Windows CI with wazero** — new territory vs modernc; CI matrix is the guard.
- **Embedding dims are corpus-fixed** — switching embed models means re-ingesting into a fresh db; enforced + documented.

## Out of scope

- PDFs, HTML, source-code ingestion; OCR.
- Web-side mutations, auth, multi-user, deployment, Litestream (the v2 "team mode" arc).
- ANN indexes / scale beyond ~100k chunks (sqlite-vec KNN is exact and fine at POC scale).
- Conversation memory / multi-turn chat; v1 ask is single-shot Q→A.
- Cross-encoder reranking, query rewriting, tokenizer-accurate chunking.
