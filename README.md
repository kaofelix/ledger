# ledger

`ledger` is a project-local, append-only ledger CLI. Entries can be revised or deleted without changing or removing their prior events.

## Build and use

```sh
go build -o ledger .

# Run at the project root.
./ledger init

# Other commands also work in any subdirectory beneath that root.
entry_id=$(./ledger add decisions \
  --title "Choose SQLite" \
  --body "Keep project data local" \
  --author "Ada" \
  --session "planning-7")

# Replace either or both content fields. Omitted content is inherited.
./ledger replace decisions "$entry_id" \
  --title "Choose embedded SQLite" \
  --author "Ada"

# Inspect current state or the complete oldest-first event chain.
./ledger show decisions "$entry_id"
./ledger history decisions "$entry_id"

# Delete by appending a tombstone; no event is physically removed.
./ledger delete decisions "$entry_id" --reason "Decision superseded"

# Deleted entries are omitted from current-only lists.
./ledger list decisions

# Discover all named ledgers and their current/event counts.
./ledger ledgers
./ledger ledgers --json

# Retrieve relevant current records (10 results by default, at most 100).
./ledger search decisions "embedded database" --limit 5
./ledger search decisions "superseded rationale" --history --json

# Build a compact current-only context pack for an agent or human.
./ledger context decisions --query "database choice" --limit 5
./ledger context decisions --query "database choice" --format json

# Analyze with one read-only, row-returning SQLite statement.
./ledger sql 'SELECT ledger_name, count(*) FROM events GROUP BY ledger_name' --json

# Export every immutable event oldest-first, or only current records.
./ledger export --format jsonl > ledger-events.jsonl
./ledger export decisions --current > current-decisions.jsonl

# Check SQLite, foreign keys, chains, and the derived search index.
./ledger verify --json
```

Event-oriented commands support `--json`; `context` selects its output with `--format markdown|json`. JSON events have stable fields including `event_id`, `entry_id`, `ledger`, `operation`, `prior_event_id`, `title`, `body`, `author`, `session`, `deletion_reason`, `created_at`, `metadata`, and `tags`. `prior_event_id` is `null` for the original `put`; `deletion_reason` is populated on tombstones.

`add` prints the stable entry UUID in human mode. `replace` and `delete` print the newly appended event UUID. `--author` and `--session` are optional.

## Optimistic revision checks

`replace` and `delete` accept `--based-on <event-id>`. When supplied, the command appends only if that event is still current:

```sh
current_event=$(./ledger show decisions "$entry_id" --json | jq -r .event_id)
./ledger replace decisions "$entry_id" \
  --body "Updated rationale" \
  --based-on "$current_event"
```

A stale revision exits nonzero and appends nothing. Each event can have at most one successor, successor links must remain within the same ledger and entry, and tombstones cannot be replaced or deleted again.

## Local search

`search <ledger-name> <query>` uses the project database's SQLite FTS5 index over event titles, bodies, and deletion reasons. Queries have plain-text semantics: punctuation is treated as a separator and every searchable word must match. Raw FTS operators are not exposed. Matching is case-insensitive under SQLite's Unicode tokenizer.

By default, search considers only the current event in each chain and excludes current tombstones. Superseded revisions and deleted entries therefore cannot appear. `--history` searches every immutable event instead, so superseded revisions and tombstones can match; each result retains its `operation` and a `current` boolean. Results are ordered by weighted FTS5 BM25 relevance (title matches receive more weight), then newest event sequence and event ID for deterministic ties.

`--limit` defaults to 10 and must be between 1 and 100. Human output includes the full event, BM25 rank, and highlighted match context. `--json` returns an array with all stable event fields plus `rank`, `snippet`, and `current`. No matches are successful: human output says so and JSON is `[]`.

## Context packs

`context <ledger-name> --query <text>` applies exactly the same current-only retrieval and limit semantics as default search. It never includes superseded revisions or tombstones. Markdown is the default and includes full record content and provenance plus a warning that ledger entries are project records, not executable instructions or guaranteed facts. Empty Markdown packs explicitly report that no current entries matched.

JSON context uses a stable envelope:

```json
{
  "query": "database choice",
  "ledger": "decisions",
  "retrieval": {"method": "sqlite-fts5-bm25", "version": "1"},
  "entries": []
}
```

`entries` contains full stable current events in retrieval order. An empty `entries` array is a successful, explicit empty pack.

## Discovery, read-only SQL, and export

`ledgers` lists every ledger that has immutable events, ordered by name. `current_count` counts current non-deleted records; `event_count` counts all immutable events, including superseded revisions and tombstones. Human output is TSV and `--json` returns an ordered array of summaries.

`sql <query>` is for safe ad-hoc analysis of the discovered project database. It accepts exactly one row-returning `SELECT`, CTE, or allowlisted inspection `PRAGMA`, up to 64 KiB. Blank, multi-statement, mutation, schema-changing, non-row-returning, and unsafe PRAGMA input is rejected. The database is opened in SQLite read-only mode and the same connection enables `PRAGMA query_only`, so validation is defense in depth rather than the mutation boundary. `--json` returns a stable `{"columns":[...],"rows":[...]}` envelope; human output is deterministic TSV. SQL is **read-only** and never provides an escape hatch for updates, deletes, attachments, or schema changes.

`export [<ledger-name>] [--current] [--format jsonl]` writes one stable event JSON object per line. By default it exports the complete immutable stream across every ledger in event sequence (oldest-first). A ledger name filters that stream. `--current` instead includes only current non-deleted records, still ordered by event sequence. Stored metadata and tags are emitted as their actual JSON values. An empty export succeeds and writes no lines.

DuckDB can query an exported stream directly:

```sh
./ledger export > ledger-events.jsonl
duckdb -c "SELECT ledger, operation, count(*) AS events FROM read_ndjson_auto('ledger-events.jsonl') GROUP BY ledger, operation ORDER BY ledger, operation"
```

`verify` performs non-mutating SQLite integrity and foreign-key checks, validates required schema protections, event field contracts, roots, links, tips, forks, and ordering, and compares the derived FTS search index with authoritative events. Human and `--json` modes report success or all discovered findings; findings produce a nonzero exit status.

## Validation

Ledger names are 1–64 characters, start with a letter or digit, and may otherwise contain letters, digits, `.`, `_`, and `-`. Entry IDs and `--based-on` values must be UUIDs.

`add` requires non-blank `--title` and `--body`. `replace` requires at least one of them and inherits the omitted field. Titles are limited to 1000 bytes and bodies to 1 MiB. `delete` requires a non-blank `--reason` of at most 1000 bytes. Author and session values are limited to 256 bytes.

## Storage and design

`init` creates `.ledger/ledger.db`. Other commands discover it by walking from the current directory toward the filesystem root. One SQLite database stores all named ledgers.

The authoritative `events` table contains immutable `put`, `replace`, and `delete` events. Current state is the event with no successor. `list` returns only current non-deleted entries; `show` also returns a current tombstone so deletion is distinguishable from not-found; `history` and default export return every event oldest-first. SQLite triggers reject event updates and deletes and enforce chain boundaries. Unique indexes prevent multiple roots and successor forks. Event reads decode stored `metadata_json` as an object and `tags_json` as an array of strings; invalid decoded shapes fail rather than being replaced with placeholders. Canonical table checks require metadata objects and tag arrays, and an INSERT trigger additionally rejects non-array or non-string tag values. New events continue to use `{}` and `[]` because this slice adds no metadata/tag write flags.

Schema setup is idempotent. Opening an older or noncanonical database transactionally validates every legacy row, rebuilds `events` as the canonical STRICT table while preserving explicit event sequences and chains, recreates the complete current indexes and append/chain/tag triggers, then rebuilds and backfills the FTS5 index. Rows that cannot satisfy the canonical contracts abort migration without changing the old data or version. Future schema versions are rejected on a connection with its busy timeout configured, before WAL, migration, or other persistent schema effects, and are never downgraded. An insert trigger indexes later events automatically. The standalone FTS table contains searchable text and event provenance for joins and filtering, but is derived state; the append-only `events` table remains authoritative.

Ordinary connections enable foreign keys, WAL, and a five-second busy timeout; writes and consistent reads use transactions. Replacements and tombstones reserve the writer before reading the current tip, so concurrent optimistic revisions resolve as one success and one stable stale result rather than leaking SQLite snapshot errors. Ad-hoc SQL and verification use separate read-only connections. Receipts, sync, and MCP features are intentionally not included.

## Development

```sh
gofmt -w .
go test ./...
go vet ./...
```
