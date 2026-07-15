# Plan Review Log: askdocs — RAG over any folder, one binary + one SQLite file

Act 1 (grill) complete — plan locked with the user (Q1–Q4 answered explicitly; Q5–Q7 delegated to Claude's recommendations and recorded in chat + PLAN.md). MAX_ROUNDS=5.

## Round 1 — Codex (gpt-5.6-sol, session 019f63c5-79f6-7882-a8b5-dcf7e7890360)

The repo contains no implementation yet, so this review targets [PLAN.md](/Users/ray_cho/go-htmx-sqlite/askdocs/PLAN.md:18). Material flaws remain:

1. **Costly GET endpoints are CSRF-able.** Any webpage can trigger `/hybrid` or `/ask/stream`, causing API spend and document egress despite the “no CSRF surface” claim.  
   Fix: Initiate paid operations via Origin-validated POST and protect the SSE GET with a one-use capability token.

2. **SSE reconnection can repeatedly bill the user.** The HTMX SSE extension reconnects automatically, while the plan explicitly says reconnecting re-asks.  
   Fix: Send the extension’s close event, assign request IDs, and deduplicate or cancel repeated streams.

3. **Streamed output creates an XSS path.** HTMX swaps SSE data as HTML, so model tokens, rendered Markdown, citations, and FTS snippets cannot be trusted as raw fragments.  
   Fix: Append tokens with `textContent` and sanitize/escape Markdown, URLs, and highlight snippets before producing HTML.

4. **`WriteTimeout=0` exposes an unbounded slow-client resource sink.** The ten-minute context is a total timeout, not an idle timeout, and no concurrency limit exists for expensive asks.  
   Fix: Add per-write deadlines, a resettable idle timer, query-size limits, and a bounded ask semaphore.

5. **“Local-first” obscures mandatory document egress.** Ingestion sends every accepted document to an external embedding endpoint, including hidden files because only hidden directories are skipped.  
   Fix: Skip hidden files by default and require an explicit egress warning/confirmation showing the endpoint and selected files.

6. **Incremental replacement is not defined atomically.** A writer consuming individual embedded chunks can delete old data and commit a partial replacement when a later embedding fails.  
   Fix: Accumulate a complete file result and replace its file, chunk, FTS, and vector rows in one transaction only after all embeddings succeed.

7. **Pruning can destroy valid data after scan errors.** An unreadable directory, transient I/O failure, or cancelled walk is indistinguishable from deletion.  
   Fix: Build a complete manifest first and prune only after a fully successful scan.

8. **Concurrent ingests can race stale manifests and prune each other’s work.** SQLite’s single-writer serialization does not protect the scan/embed phase.  
   Fix: Hold an exclusive application-level ingest lock for the entire manifest-to-commit lifecycle.

9. **Hashing and chunking can observe different file contents.** Separate reads permit storing a hash for one version and chunks from another, potentially causing permanent false “unchanged” results.  
   Fix: Read each file once into bounded memory and derive both its hash and chunks from those exact bytes.

10. **Corpus identity is stored but not enforced.** Reusing a DB with another root causes relative-path collisions and mass pruning; additionally, `./askdocs.db` is not beside an absolute ingest path.  
    Fix: Canonicalize and enforce `corpus_root`, and define the ingest default DB as a path derived from that root.

11. **Vector schema initialization is circular.** `vec0` needs dimensions during creation, but dimensions are learned only from the first embedding response.  
    Fix: Migrate base tables first, then atomically create and stamp the vector table after validating the first response.

12. **The sqlite-vec initialization contract is missing.** Selecting the binding dependency alone does not register `vec0` or embed the ncruces SQLite WASM module on every connection.  
    Fix: Specify extension registration before opening the pool and require a startup `vec_version()`/`CREATE vec0` smoke test.

13. **Model name and dimensions do not identify an embedding space.** Changing `OPENAI_BASE_URL` while retaining the same model name can silently mix incompatible vectors.  
    Fix: Stamp and validate an explicit embedding-provider/fingerprint identifier alongside model and dimensions.

14. **Embedding responses are insufficiently validated.** OpenAI responses carry indices and can be missing, reordered, duplicated, wrong-dimensional, or non-finite.  
    Fix: Reorder by index and reject incorrect counts, duplicate indices, dimension mismatches, non-finite values, and oversized bodies.

15. **Raw FTS5 queries are not ordinary user text.** Quotes, operators, column filters, empty strings, and pathological expressions can produce errors or excessive work.  
    Fix: Convert user input into a bounded literal-token query and handle empty/invalid searches without executing `MATCH`.

16. **Hybrid result semantics are incomplete.** Fixed top-20 candidate pools cannot honor arbitrary `limit`, and ties will be nondeterministic if fused through Go maps.  
    Fix: Validate/cap limits, size candidate pools accordingly, and define stable score/rank/ID tie-breaking.

17. **Read commands may mutate or create the wrong database.** `user_version` alone does not distinguish an empty foreign SQLite file, and a mistyped path may silently create one.  
    Fix: Use `application_id`/schema magic, create only during ingest, and open search/status/ask/web in existing read-only mode.

18. **The documented CLI syntax conflicts with Go’s standard `flag` parser.** Flags after `<path>` or `<query>` will not be parsed by a normal `FlagSet`.  
    Fix: Put flags before positional arguments in the contract or implement and test interspersed-flag parsing.

19. **Ollama compatibility contradicts mandatory API-key validation.** Local OpenAI-compatible endpoints commonly require no key, and base-URL joining/default behavior is unspecified.  
    Fix: Make authentication optional, define `/v1` URL resolution precisely, and add a keyless Ollama compatibility test.

20. **Retry behavior can amplify outages.** Four workers retrying all 429/5xx responses without limits, jitter, or `Retry-After` handling can create a retry storm.  
    Fix: Use bounded context-aware retries with exponential jitter, `Retry-After`, body limits, and coordinated rate limiting.

21. **Citation correctness is assumed, not enforced.** The model may emit nonexistent citations or obey instructions embedded in retrieved documents.  
    Fix: Delimit context as untrusted data and validate every rendered citation against the supplied source set.

22. **The SSE test cannot prove streaming.** A `ResponseRecorder` checked after completion does not verify flushing, early delivery, reconnection, or disconnect cancellation.  
    Fix: Test through `httptest.Server`, read an event before handler completion, then disconnect and assert upstream cancellation.

23. **The fallback is not schema-compatible and defeats the core thesis.** Replacing `vec0` with BLOBs plus Go cosine requires a schema/search migration and removes vector search from SQLite.  
    Fix: Make cross-platform sqlite-vec support a blocking spike; if it fails, explicitly revise the schema and product claim.

24. **The “one file” claim conflicts with WAL operation.** WAL creates `-wal` and `-shm` files, while status counts only the main DB and deletion guidance names one file.  
    Fix: Qualify the claim, report sidecar sizes, checkpoint on clean close, and document deleting the complete DB file set.

25. **Failure observability is underspecified.** Aggregate progress cannot identify which file, batch, upstream status, or pipeline stage failed.  
    Fix: Emit bounded stderr diagnostics with stage, file, attempt, status, latency, and upstream request ID while excluding content and credentials.

VERDICT: REVISE

VERDICT: REVISE

### Claude's response (round 1)

All 16 findings accepted — four via simpler mechanisms than proposed:

- #1 via **Sec-Fetch-Site middleware** instead of POST+capability-token: EventSource cannot POST, and the header check (same-origin/none/absent allowed, cross-site 403) closes the cross-site-GET spend hole in one middleware with no state. Residual: pre-2020 browsers, acceptable for a localhost dev tool.
- #2 accepted with the htmx-native fix: the final `done` SSE event replaces the element carrying `sse-connect`, removing the EventSource so auto-reconnect (and re-billing) is structurally impossible.
- #3 accepted: token events HTML-escaped server-side; citations/done fragments via html/template; goldmark raw-HTML-off.
- #4 accepted via `http.ResponseController` per-write deadlines + 10-min stream cap + ask semaphore (2) + 512-char q cap, instead of a blanket WriteTimeout=0.
- #5 accepted as **printed egress notice** (provider host + file/chunk counts before any embedding call) + hidden-file skipping; interactive confirmation rejected to keep ingest scriptable — README documents egress; Ollama is the air-gap path.
- #6 accepted: per-file atomic replacement, committed only after all of a file's embeddings succeed.
- #7 accepted: prune runs only after a complete error-free walk.
- #8 accepted via pid-stamped `O_EXCL` lockfile for the scan→commit lifecycle (a DB-level lock can't span the non-DB phases).
- #9 accepted: read each file once; hash and chunks from the same bytes.
- #10 accepted: canonical `corpus_root` stamped and enforced; ingest default db moved to `<corpus-root>/askdocs.db`; walker skips the db + sidecars.
- #11 accepted: two-phase schema — base tables at open, vec0 created lazily at first validated embedding batch, stamped atomically; pre-embedding hybrid/ask errors cleanly.
- #12 accepted: binding import contract stated + `SELECT vec_version()` smoke test at every open.
- #13 accepted: `embed_provider_host` stamped alongside model+dims; mismatch is a hard error with ASKDOCS_ALLOW_PROVIDER_MISMATCH=1 escape.
- #14 accepted: strict embedding-response validation (index reorder, count, dims, finite).
- #15 accepted: til's FTS query builder explicitly ported (was implicit).
- #16 accepted: limit clamp 1..50, pool max(20, 4×limit)≤100, deterministic tie-break (score desc, id asc).

### Claude's response, addendum (findings 17–25 — the round-1 critique had 25 findings, not 16; the earlier extraction truncated)

All nine additional findings accepted:
- #17: only `ingest` creates a db; every other command requires an existing file and opens read-only; `PRAGMA application_id` ("ASKD") stamped and verified — foreign/empty SQLite files error instead of being migrated.
- #18: CLI contract fixed to flags-before-positionals (`askdocs ingest [-db f] <path>`), matching Go flag semantics; help text consistent.
- #19: `OPENAI_API_KEY` optional when base host is loopback (keyless Ollama); Authorization header only when set; base URL taken verbatim (must include `/v1`), client appends only endpoint paths; keyless compatibility test added.
- #20: retries bounded (≤3, exponential + jitter, Retry-After honored, ctx-aware, 10MB body cap).
- #21: context delimited as untrusted data in the prompt; rendered [n] citations validated against the actual source set (out-of-range → inert text). Full prompt-injection defense acknowledged as residual risk.
- #22: SSE tested via real httptest.Server — first event asserted before handler completion; client disconnect asserted to cancel the upstream fake.
- #23: sqlite-vec promoted to a day-one blocking spike through the 3-OS CI matrix; fallback re-characterized as an explicit plan revision (schema change + weaker claim), not a drop-in.
- #24: `wal_checkpoint(TRUNCATE)` on clean close; status reports sidecar sizes; docs qualify the one-file claim (`askdocs.db*` set).
- #25: per-failure bounded stderr diagnostics (stage, file, attempt, status, latency; no content/credentials) + end-of-run failure summary.

## Round 2 — Codex (same session)

The simpler choices for #1, #5, #8, and the provider-host stamp are sound for a localhost single-user POC.

Round-1 status:

- Adequately resolved: #1, #3–#12, #14–#16, #18–#23, #25.
- Still incomplete: #2, #13, #17, and #24.

Material remaining issues:

1. **#2 only prevents reconnect after successful `done`.** A mid-stream network/upstream failure occurs before DOM replacement, so EventSource/HTMX reconnects automatically and re-bills; “manual retry” is incorrect.  
   Fix: Remove the connector on the HTMX SSE error event or use a one-use request ID, and test an actual reconnect.

2. **#17 conflicts with the migration design.** Read-only commands cannot execute the stated `BEGIN IMMEDIATE` migration performed on “every Open.”  
   Fix: Split writable `OpenIngest` from `OpenReadOnly`; read-only opens only verify application/schema versions.

3. **#24 conflicts with read-only stores.** Unconditionally running `wal_checkpoint(TRUNCATE)` during `Store.Close()` fails on read-only search/status/ask/web connections.  
   Fix: Checkpoint only writable ingest stores and treat reader-induced `BUSY` as a documented best-effort outcome.

4. **#13’s escape-hatch semantics are unsafe as described.** Allowing a provider mismatch during incremental ingestion can mix old and new embedding spaces; that is not a migration.  
   Fix: Define the override strictly as an assertion of equivalent embedding space; otherwise require a full rebuild and never merely restamp metadata.

5. **SSE HTML escaping does not provide valid SSE framing.** Tokens and rendered fragments commonly contain newlines, which must each receive a `data:` prefix or they can truncate/inject events.  
   Fix: Centralize standards-compliant SSE encoding and test multiline data containing blank lines and `event:` text.

6. **Oversized paragraphs still bypass the chunk limit.** A single long paragraph or code block can approach 1 MB and exceed embedding limits despite the 2,000-character target.  
   Fix: Hard-split oversized paragraphs rune-safely and cap overlap size.

7. **Remote plaintext endpoints are accepted with API keys.** `http://non-loopback` leaks credentials and document contents.  
   Fix: Require HTTPS except for loopback, with an explicit insecure override if genuinely needed.

8. **Chat output remains unbounded.** The web handler accumulates the answer for the final Markdown fragment, while CLI streams have no stated lifetime or byte limit.  
   Fix: Set a completion-token limit plus a hard accumulated-byte/event-size cap and cancel upstream when exceeded.

VERDICT: REVISE

### Claude's response (round 2)

Codex confirmed the round-1 simpler mechanisms (#1/#5/#8/#13-stamp) sound, and 21 of 25 findings resolved. All 8 round-2 findings accepted:
1. Terminal event on every outcome (success AND upstream error) replaces the connector; one-use nonce with in-memory TTL set guards true network-drop reconnects — replayed nonce gets a terminal event with zero LLM calls; real reconnect tested.
2. Open paths split: OpenIngest (writable, migrates in BEGIN IMMEDIATE) vs OpenReadOnly (stat-first, read-only flag, verify-only).
3. wal_checkpoint(TRUNCATE) only on writable close, best-effort on BUSY.
4. Model/dims mismatch: hard error, NO override (fresh re-ingest only). Provider-host override redefined as an equivalence assertion that restamps host while model+dims must still match.
5. Central sseWrite encoder with per-line data: framing; multiline/blank/event:-text payload tests.
6. Oversized single paragraphs hard-split rune-safely at 2000 chars; overlap capped 500.
7. https required for non-loopback endpoints; ASKDOCS_ALLOW_INSECURE=1 explicit override.
8. max_tokens on chat requests (default 1024) + 256KB accumulation cap → upstream cancel + error-panel terminal event.

## Round 3 — Codex (same session)

The round-2 mechanisms are sound, but several were not incorporated consistently:

1. **Read-only migration contradiction remains.** `OpenReadOnly` is verify-only, but Phase 1 still says migration runs on “every Open.”  
   Fix: Change Phase 1 to “every `OpenIngest`.”

2. **Checkpoint contradiction remains.** The store section limits checkpointing to writable closes, while the CLI section still says every `Store.Close()` checkpoints.  
   Fix: State that only writable ingest stores checkpoint.

3. **Provider restamping conflicts with read-only commands.** A provider override cannot restamp metadata through search/ask/web connections.  
   Fix: Allow restamping only through `OpenIngest`; require one override-enabled ingest before using the new equivalent provider.

4. **The dynamic SSE connection flow is missing.** The ask form needs a fresh nonce and query-specific `sse-connect` URL, but no connector-fragment route or client setup is defined.  
   Fix: Add `/ask/connect?q=` returning connector markup with a fresh nonce, then connect it to `/ask/stream`.

5. **Key-decision text is stale.** It still claims no server state, stateless reconnects, and provider “migration,” contradicting the nonce TTL set and equivalence-only override.  
   Fix: Update those tradeoff bullets to match the implemented mechanisms.

No additional security or data-integrity flaw rises above the POC bar.

VERDICT: REVISE

### Claude's response (round 3)

All five consistency findings accepted and applied:
1. Phase 1 migration scoped to "every OpenIngest"; OpenReadOnly never migrates.
2. Checkpoint language unified: writable ingest close only.
3. Provider restamp only via override-enabled ingest; read-only commands keep erroring until then.
4. Added GET /ask/connect?q= — mints + registers the nonce and returns the sse-connect connector fragment; the ask form targets it.
5. Key-decision bullets rewritten to match: terminal-event + nonce (the app's only, ephemeral, server-side state), equivalence-assertion override, deliberate-retry-through-connect semantics.

## Round 4 — Codex (same session)

Confirmed all five round-3 changes are consistently incorporated. No new material security, integrity, or implementation blockers remain for the localhost single-user POC.

Implementation note: consume the issued nonce atomically on first `/ask/stream` request; the replay test already implies this behavior.

VERDICT: APPROVED

---
Converged in 4 rounds (25 + 8 + 5 findings resolved). Plan locked for implementation pending user sign-off.
