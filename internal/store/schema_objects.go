package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const canonicalEventsTableSQL = `
CREATE TABLE IF NOT EXISTS events (
    event_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE CHECK(length(event_id) = 36),
    entry_id TEXT NOT NULL CHECK(length(entry_id) = 36),
    ledger_name TEXT NOT NULL CHECK(length(ledger_name) BETWEEN 1 AND 64),
    operation TEXT NOT NULL CHECK(operation IN ('put', 'replace', 'delete')),
    prior_event_id TEXT REFERENCES events(event_id),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    author TEXT,
    session TEXT,
    deletion_reason TEXT,
    created_at TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metadata_json) AND json_type(metadata_json) = 'object'),
    tags_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(tags_json) AND json_type(tags_json) = 'array'),
    CHECK(
        (operation = 'put' AND prior_event_id IS NULL) OR
        (operation IN ('replace', 'delete') AND prior_event_id IS NOT NULL)
    )
) STRICT`

type schemaObjectSpec struct {
	objectType string
	name       string
	sql        string
}

var protectiveSchemaObjects = []schemaObjectSpec{
	{"index", "events_one_successor", `CREATE UNIQUE INDEX IF NOT EXISTS events_one_successor ON events(prior_event_id) WHERE prior_event_id IS NOT NULL`},
	{"index", "events_one_root", `CREATE UNIQUE INDEX IF NOT EXISTS events_one_root ON events(ledger_name, entry_id) WHERE operation = 'put'`},
	{"index", "events_current_ledger", `CREATE INDEX IF NOT EXISTS events_current_ledger ON events(ledger_name, event_seq DESC)`},
	{"trigger", "events_chain_invariants", `
		CREATE TRIGGER IF NOT EXISTS events_chain_invariants
		BEFORE INSERT ON events WHEN NEW.prior_event_id IS NOT NULL
		BEGIN
			SELECT CASE WHEN NOT EXISTS (
				SELECT 1 FROM events prior
				WHERE prior.event_id = NEW.prior_event_id
				  AND prior.entry_id = NEW.entry_id
				  AND prior.ledger_name = NEW.ledger_name
				  AND prior.operation <> 'delete'
			) THEN RAISE(ABORT, 'invalid event chain') END;
		END`},
	{"trigger", "events_append_only_update", `
		CREATE TRIGGER IF NOT EXISTS events_append_only_update
		BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT, 'events are append-only'); END`},
	{"trigger", "events_append_only_delete", `
		CREATE TRIGGER IF NOT EXISTS events_append_only_delete
		BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT, 'events are append-only'); END`},
	{"trigger", "events_tags_are_strings", `
		CREATE TRIGGER IF NOT EXISTS events_tags_are_strings
		BEFORE INSERT ON events
		WHEN CASE
			WHEN json_valid(NEW.tags_json) != 1 THEN 1
			WHEN json_type(NEW.tags_json) <> 'array' THEN 1
			WHEN EXISTS (SELECT 1 FROM json_each(NEW.tags_json) WHERE type <> 'text') THEN 1
			ELSE 0
		END = 1
		BEGIN SELECT RAISE(ABORT, 'tags must be an array of strings'); END`},
}

var ftsTableSpec = schemaObjectSpec{"table", "event_search", `
	CREATE VIRTUAL TABLE IF NOT EXISTS event_search USING fts5(
		title, body, deletion_reason,
		event_seq UNINDEXED, event_id UNINDEXED, entry_id UNINDEXED,
		ledger_name UNINDEXED, operation UNINDEXED,
		tokenize = 'unicode61'
	)`}

var ftsTriggerSpec = schemaObjectSpec{"trigger", "events_search_insert", `
	CREATE TRIGGER IF NOT EXISTS events_search_insert
	AFTER INSERT ON events
	BEGIN
		INSERT INTO event_search(
			rowid, title, body, deletion_reason, event_seq,
			event_id, entry_id, ledger_name, operation
		) VALUES (
			NEW.event_seq, NEW.title, NEW.body, NEW.deletion_reason, NEW.event_seq,
			NEW.event_id, NEW.entry_id, NEW.ledger_name, NEW.operation
		);
	END`}

const backfillFTSSQL = `
	INSERT INTO event_search(
		rowid, title, body, deletion_reason, event_seq,
		event_id, entry_id, ledger_name, operation
	)
	SELECT event_seq, title, body, deletion_reason, event_seq,
	       event_id, entry_id, ledger_name, operation
	FROM events
	WHERE NOT EXISTS (SELECT 1 FROM event_search WHERE event_search.rowid = events.event_seq)`

type schemaDefinitionQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func normalizedSchemaSQL(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "if not exists", "")
	value = strings.ReplaceAll(value, `"`, "")
	value = strings.ReplaceAll(value, ";", "")
	return strings.Join(strings.Fields(value), " ")
}

func schemaObjectMatches(db schemaDefinitionQuerier, spec schemaObjectSpec) (bool, error) {
	var objectType, definition string
	err := db.QueryRowContext(context.Background(), `SELECT type, sql FROM sqlite_master WHERE name = ?`, spec.name).Scan(&objectType, &definition)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect schema object %s: %w", spec.name, err)
	}
	return objectType == spec.objectType && normalizedSchemaSQL(definition) == normalizedSchemaSQL(spec.sql), nil
}

func canonicalEventsTableMatches(db schemaDefinitionQuerier) (bool, error) {
	return schemaObjectMatches(db, schemaObjectSpec{"table", "events", canonicalEventsTableSQL})
}

func allSafeSchemaObjectsMatch(db schemaDefinitionQuerier) (bool, error) {
	tableMatches, err := canonicalEventsTableMatches(db)
	if err != nil || !tableMatches {
		return false, err
	}
	for _, spec := range protectiveSchemaObjects {
		matches, err := schemaObjectMatches(db, spec)
		if err != nil || !matches {
			return false, err
		}
	}
	for _, spec := range []schemaObjectSpec{ftsTableSpec, ftsTriggerSpec} {
		matches, err := schemaObjectMatches(db, spec)
		if err != nil || !matches {
			return false, err
		}
	}
	return true, nil
}

func rebuildCanonicalEventsTable(tx *sql.Tx) error {
	if _, err := tx.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS events_search_insert; DROP TABLE IF EXISTS event_search`); err != nil {
		return fmt.Errorf("remove derived search schema before events rebuild: %w", err)
	}
	for _, spec := range protectiveSchemaObjects {
		if _, err := tx.ExecContext(context.Background(), fmt.Sprintf(`DROP %s IF EXISTS %s`, strings.ToUpper(spec.objectType), spec.name)); err != nil {
			return fmt.Errorf("drop schema object %s before events rebuild: %w", spec.name, err)
		}
	}
	replacementSQL := strings.Replace(canonicalEventsTableSQL, "IF NOT EXISTS events", "events_v4", 1)
	replacementSQL = strings.Replace(replacementSQL, "REFERENCES events(event_id)", "REFERENCES events_v4(event_id)", 1)
	if _, err := tx.ExecContext(context.Background(), replacementSQL); err != nil {
		return fmt.Errorf("create canonical events replacement: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), `
		INSERT INTO events_v4(
			event_seq, event_id, entry_id, ledger_name, operation, prior_event_id,
			title, body, author, session, deletion_reason, created_at, metadata_json, tags_json
		)
		SELECT event_seq, event_id, entry_id, ledger_name, operation, prior_event_id,
		       title, body, author, session, deletion_reason, created_at, metadata_json, tags_json
		FROM events ORDER BY event_seq`); err != nil {
		return fmt.Errorf("copy events into canonical table: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), `DROP TABLE events; ALTER TABLE events_v4 RENAME TO events`); err != nil {
		return fmt.Errorf("replace legacy events table: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), `
		DELETE FROM sqlite_sequence WHERE name IN ('events', 'events_v4');
		INSERT INTO sqlite_sequence(name, seq)
		SELECT 'events', max(event_seq) FROM events HAVING count(*) > 0`); err != nil {
		return fmt.Errorf("restore event sequence: %w", err)
	}
	return nil
}

func canonicalizeSafeSchema(tx *sql.Tx) error {
	for _, spec := range protectiveSchemaObjects {
		if _, err := tx.ExecContext(context.Background(), fmt.Sprintf(`DROP %s IF EXISTS %s`, strings.ToUpper(spec.objectType), spec.name)); err != nil {
			return fmt.Errorf("drop schema object %s: %w", spec.name, err)
		}
		if _, err := tx.ExecContext(context.Background(), spec.sql); err != nil {
			return fmt.Errorf("create schema object %s: %w", spec.name, err)
		}
	}
	ftsCompatible, err := schemaObjectMatches(tx, ftsTableSpec)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS events_search_insert`); err != nil {
		return fmt.Errorf("drop FTS trigger: %w", err)
	}
	if !ftsCompatible {
		if _, err := tx.ExecContext(context.Background(), `DROP TABLE IF EXISTS event_search`); err != nil {
			return fmt.Errorf("drop incompatible FTS table: %w", err)
		}
		if _, err := tx.ExecContext(context.Background(), ftsTableSpec.sql); err != nil {
			return fmt.Errorf("create FTS table: %w", err)
		}
	}
	if _, err := tx.ExecContext(context.Background(), ftsTriggerSpec.sql); err != nil {
		return fmt.Errorf("create FTS trigger: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), backfillFTSSQL); err != nil {
		return fmt.Errorf("backfill FTS table: %w", err)
	}
	return nil
}
