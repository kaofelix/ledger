package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenMigratesSliceOneDatabaseIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE events (
			event_seq INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL UNIQUE,
			entry_id TEXT NOT NULL,
			ledger_name TEXT NOT NULL,
			operation TEXT NOT NULL,
			prior_event_id TEXT REFERENCES events(event_id),
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			author TEXT,
			session TEXT,
			created_at TEXT NOT NULL,
			metadata_json TEXT NOT NULL DEFAULT '{}',
			tags_json TEXT NOT NULL DEFAULT '[]'
		);
		INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002', 'old', 'put', 'kept', 'body', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		migrated, err := open(path)
		if err != nil {
			t.Fatalf("migration attempt %d: %v", attempt+1, err)
		}
		var title string
		var reason sql.NullString
		if err := migrated.QueryRow(`SELECT title, deletion_reason FROM events`).Scan(&title, &reason); err != nil {
			t.Fatalf("read migrated event: %v", err)
		}
		migrated.Close()
		if title != "kept" || reason.Valid {
			t.Fatalf("migration changed event: title=%q reason=%v", title, reason)
		}
	}

	migrated, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var requiredObjects int
	if err := migrated.QueryRow(`
		SELECT count(*) FROM sqlite_master WHERE name IN (
			'event_search', 'events_one_successor', 'events_one_root', 'events_current_ledger',
			'events_chain_invariants', 'events_append_only_update', 'events_append_only_delete',
			'events_tags_are_strings', 'events_search_insert'
		)`).Scan(&requiredObjects); err != nil {
		t.Fatal(err)
	}
	if requiredObjects != 9 {
		t.Fatalf("migration installed %d required objects, want 9", requiredObjects)
	}
	assertRejected := func(label, query string, args ...any) {
		t.Helper()
		if _, err := migrated.Exec(query, args...); err == nil {
			t.Fatalf("%s unexpectedly succeeded", label)
		}
	}
	assertRejected("UPDATE", `UPDATE events SET title = 'changed'`)
	assertRejected("DELETE", `DELETE FROM events`)
	assertRejected("duplicate root", `
		INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000003', '00000000-0000-4000-8000-000000000002', 'old', 'put', 'duplicate', 'body', '2026-01-02T00:00:00Z')`)
	assertRejected("cross-entry chain", `
		INSERT INTO events (event_id, entry_id, ledger_name, operation, prior_event_id, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000004', '00000000-0000-4000-8000-000000000099', 'old', 'replace', '00000000-0000-4000-8000-000000000001', 'bad', 'body', '2026-01-02T00:00:00Z')`)
	assertRejected("cross-ledger chain", `
		INSERT INTO events (event_id, entry_id, ledger_name, operation, prior_event_id, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000005', '00000000-0000-4000-8000-000000000002', 'other', 'replace', '00000000-0000-4000-8000-000000000001', 'bad', 'body', '2026-01-02T00:00:00Z')`)
	if _, err := migrated.Exec(`
		INSERT INTO events (event_id, entry_id, ledger_name, operation, prior_event_id, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000006', '00000000-0000-4000-8000-000000000002', 'old', 'replace', '00000000-0000-4000-8000-000000000001', 'next', 'body', '2026-01-02T00:00:00Z')`); err != nil {
		t.Fatalf("valid successor: %v", err)
	}
	assertRejected("fork", `
		INSERT INTO events (event_id, entry_id, ledger_name, operation, prior_event_id, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000007', '00000000-0000-4000-8000-000000000002', 'old', 'replace', '00000000-0000-4000-8000-000000000001', 'fork', 'body', '2026-01-03T00:00:00Z')`)
	if _, err := migrated.Exec(`
		INSERT INTO events (event_id, entry_id, ledger_name, operation, prior_event_id, title, body, deletion_reason, created_at)
		VALUES ('00000000-0000-4000-8000-000000000008', '00000000-0000-4000-8000-000000000002', 'old', 'delete', '00000000-0000-4000-8000-000000000006', 'next', 'body', 'done', '2026-01-03T00:00:00Z')`); err != nil {
		t.Fatalf("valid tombstone: %v", err)
	}
	assertRejected("post-tombstone successor", `
		INSERT INTO events (event_id, entry_id, ledger_name, operation, prior_event_id, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000009', '00000000-0000-4000-8000-000000000002', 'old', 'replace', '00000000-0000-4000-8000-000000000008', 'resurrected', 'body', '2026-01-04T00:00:00Z')`)
}

func TestMigrationRebuildsLegacyEventsCanonicallyAndPreservesSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE events (
			event_seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
			entry_id TEXT NOT NULL, ledger_name TEXT NOT NULL, operation TEXT NOT NULL,
			prior_event_id TEXT REFERENCES events(event_id), title TEXT NOT NULL, body TEXT NOT NULL,
			author TEXT, session TEXT, created_at TEXT NOT NULL,
			metadata_json TEXT NOT NULL DEFAULT '{}', tags_json TEXT NOT NULL DEFAULT '[]'
		);
		INSERT INTO events (event_seq, event_id, entry_id, ledger_name, operation, prior_event_id, title, body, author, session, created_at, metadata_json, tags_json)
		VALUES
			(7, '00000000-0000-4000-8000-000000000101', '00000000-0000-4000-8000-000000000102',
			 'old', 'put', NULL, 'kept', 'body', 'Ada', 'legacy', '2026-01-01T00:00:00Z', '{"source":"legacy"}', '["kept"]'),
			(9, '00000000-0000-4000-8000-000000000103', '00000000-0000-4000-8000-000000000102',
			 'old', 'replace', '00000000-0000-4000-8000-000000000101', 'revised', 'body 2', 'Ada', 'legacy', '2026-01-02T00:00:00Z', '{"revision":2}', '["kept","revised"]')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	migrated.Close()
	verification, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if !verification.OK {
		t.Fatalf("migrated database did not verify: %#v", verification.Findings)
	}
	history, err := History(path, "old", "00000000-0000-4000-8000-000000000102")
	if err != nil || len(history) != 2 || history[0].EventID != "00000000-0000-4000-8000-000000000101" || history[1].PriorEventID != history[0].EventID || history[0].Metadata["source"] != "legacy" || len(history[1].Tags) != 2 {
		t.Fatalf("legacy events or chain changed: history=%#v err=%v", history, err)
	}
	if _, err := Add(path, AddInput{Ledger: "old", Title: "next", Body: "body"}); err != nil {
		t.Fatal(err)
	}
	inspect, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close()
	var sequences string
	if err := inspect.QueryRow(`SELECT group_concat(event_seq, ',') FROM (SELECT event_seq FROM events ORDER BY event_seq)`).Scan(&sequences); err != nil {
		t.Fatal(err)
	}
	if sequences != "7,9,10" {
		t.Fatalf("event sequence continuity lost: %q", sequences)
	}
}

func TestOpenRebuildsNoncanonicalV4EventsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE events (
			event_seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
			entry_id TEXT NOT NULL, ledger_name TEXT NOT NULL, operation TEXT NOT NULL,
			prior_event_id TEXT, title TEXT NOT NULL, body TEXT NOT NULL, author TEXT, session TEXT,
			deletion_reason TEXT, created_at TEXT NOT NULL,
			metadata_json TEXT NOT NULL DEFAULT '{}', tags_json TEXT NOT NULL DEFAULT '[]'
		);
		INSERT INTO events(event_seq,event_id,entry_id,ledger_name,operation,title,body,created_at)
		VALUES(12,'00000000-0000-4000-8000-000000000111','00000000-0000-4000-8000-000000000112','v4','put','title','body','2026-01-01T00:00:00Z');
		PRAGMA user_version = 4`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	verification, err := Verify(path)
	if err != nil || !verification.OK {
		t.Fatalf("repaired v4 did not verify: findings=%#v err=%v", verification.Findings, err)
	}
}

func TestMigrationRejectsInvalidExistingChainsBeforeInstallingConstraints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE events (
			event_seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
			entry_id TEXT NOT NULL, ledger_name TEXT NOT NULL, operation TEXT NOT NULL,
			prior_event_id TEXT REFERENCES events(event_id), title TEXT NOT NULL, body TEXT NOT NULL,
			author TEXT, session TEXT, created_at TEXT NOT NULL,
			metadata_json TEXT NOT NULL DEFAULT '{}', tags_json TEXT NOT NULL DEFAULT '[]'
		);
		INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at) VALUES
			('00000000-0000-4000-8000-000000000071', '00000000-0000-4000-8000-000000000072', 'bad', 'put', 'one', 'body', '2026-01-01T00:00:00Z'),
			('00000000-0000-4000-8000-000000000073', '00000000-0000-4000-8000-000000000072', 'bad', 'put', 'two', 'body', '2026-01-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if migrated, err := open(path); err == nil || !strings.Contains(err.Error(), "invariant") {
		if migrated != nil {
			migrated.Close()
		}
		t.Fatalf("invalid migration error=%v, want invariant validation", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed legacy migration changed database bytes")
	}
	inspect, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close()
	var version, deletionColumn, protections, eventCount int
	if err := inspect.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := inspect.QueryRow(`SELECT count(*) FROM pragma_table_info('events') WHERE name = 'deletion_reason'`).Scan(&deletionColumn); err != nil {
		t.Fatal(err)
	}
	if err := inspect.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('events_one_root','events_one_successor','events_append_only_update','events_append_only_delete')`).Scan(&protections); err != nil {
		t.Fatal(err)
	}
	if err := inspect.QueryRow(`SELECT count(*) FROM events WHERE title IN ('one','two')`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if version != 0 || deletionColumn != 0 || protections != 0 || eventCount != 2 {
		t.Fatalf("failed migration was not atomic: version=%d deletion_column=%d protections=%d events=%d", version, deletionColumn, protections, eventCount)
	}
}

func TestOpenRejectsFutureSchemaWithoutChangingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE preserved(value TEXT); INSERT INTO preserved VALUES ('unchanged'); PRAGMA user_version = %d`, currentSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := open(path)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("future schema error=%v, want rejection", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("future-schema open changed database bytes")
	}
	inspect, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close()
	var version int
	var value string
	if err := inspect.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := inspect.QueryRow(`SELECT value FROM preserved`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion+1 || value != "unchanged" {
		t.Fatalf("future database changed: version=%d value=%q", version, value)
	}
}

func TestMigrationRejectsUnreadableMetadataAndTags(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		metadata string
		tags     string
	}{
		{"metadata is not object", `[]`, `[]`},
		{"tag is not string", `{}`, `["ok",7]`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`
				CREATE TABLE events (
					event_seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
					entry_id TEXT NOT NULL, ledger_name TEXT NOT NULL, operation TEXT NOT NULL,
					prior_event_id TEXT REFERENCES events(event_id), title TEXT NOT NULL, body TEXT NOT NULL,
					author TEXT, session TEXT, created_at TEXT NOT NULL,
					metadata_json TEXT NOT NULL DEFAULT '{}', tags_json TEXT NOT NULL DEFAULT '[]'
				);
				INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at, metadata_json, tags_json)
				VALUES ('00000000-0000-4000-8000-000000000093', '00000000-0000-4000-8000-000000000094',
				        'old', 'put', 'title', 'body', '2026-01-01T00:00:00Z', ?, ?)`, fixture.metadata, fixture.tags)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := open(path)
			if opened != nil {
				opened.Close()
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "metadata and tags") {
				t.Fatalf("migration error=%v, want unreadable JSON rejection", err)
			}
			inspect, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer inspect.Close()
			var version int
			if err := inspect.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			var metadata, tags string
			if err := inspect.QueryRow(`SELECT metadata_json, tags_json FROM events`).Scan(&metadata, &tags); err != nil {
				t.Fatal(err)
			}
			if version != 0 || metadata != fixture.metadata || tags != fixture.tags {
				t.Fatalf("failed JSON migration changed database: version=%d metadata=%q tags=%q", version, metadata, tags)
			}
		})
	}
}

func TestOpenBuildsAndMaintainsEventSearchIndexIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE events (
			event_seq INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL UNIQUE,
			entry_id TEXT NOT NULL,
			ledger_name TEXT NOT NULL,
			operation TEXT NOT NULL,
			prior_event_id TEXT REFERENCES events(event_id),
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			author TEXT,
			session TEXT,
			deletion_reason TEXT,
			created_at TEXT NOT NULL,
			metadata_json TEXT NOT NULL DEFAULT '{}',
			tags_json TEXT NOT NULL DEFAULT '[]'
		);
		PRAGMA user_version = 2;
		INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002', 'old', 'put', 'backfilled title', 'existing searchable body', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		migrated, err := open(path)
		if err != nil {
			t.Fatalf("migration attempt %d: %v", attempt+1, err)
		}
		var count int
		if err := migrated.QueryRow(`SELECT count(*) FROM event_search WHERE event_search MATCH 'searchable'`).Scan(&count); err != nil {
			t.Fatalf("query backfilled index: %v", err)
		}
		if count != 1 {
			t.Fatalf("backfilled matches=%d, want 1", count)
		}
		migrated.Close()
	}

	migrated, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	_, err = migrated.Exec(`
		INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000003', '00000000-0000-4000-8000-000000000004', 'old', 'put', 'later title', 'automatically indexed', '2026-01-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	var eventID, entryID, ledger, operation string
	if err := migrated.QueryRow(`
		SELECT event_id, entry_id, ledger_name, operation
		FROM event_search WHERE event_search MATCH 'automatically'`).Scan(&eventID, &entryID, &ledger, &operation); err != nil {
		t.Fatalf("query automatically indexed event: %v", err)
	}
	if eventID != "00000000-0000-4000-8000-000000000003" || entryID != "00000000-0000-4000-8000-000000000004" || ledger != "old" || operation != "put" {
		t.Fatalf("indexed provenance = %q %q %q %q", eventID, entryID, ledger, operation)
	}
}

func TestImmediateRevisionLockReportsStableConflict(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".ledger", "ledger.db")
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.Exec(`PRAGMA busy_timeout = 0; BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer holder.Exec(`ROLLBACK`)

	contender, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	contender.SetMaxOpenConns(1)
	if _, err := contender.Exec(`PRAGMA busy_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	conn, err := beginImmediate(contender)
	if conn != nil {
		closeImmediate(conn)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("lock error=%v, want stable ErrConflict", err)
	}
}

func TestConcurrentRevisionsSerializeToStableStaleResult(t *testing.T) {
	for _, operation := range []string{"replace", "delete"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			if err := Init(root); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".ledger", "ledger.db")
			original, err := Add(path, AddInput{Ledger: "race", Title: "original", Body: "body"})
			if err != nil {
				t.Fatal(err)
			}

			firstReserved := make(chan struct{})
			releaseFirst := make(chan struct{})
			var reservations atomic.Int32
			revisionReservationHook = func() {
				if reservations.Add(1) == 1 {
					close(firstReserved)
					<-releaseFirst
				}
			}
			defer func() { revisionReservationHook = func() {} }()

			results := make(chan error, 2)
			revise := func(label string) {
				if operation == "replace" {
					title := label
					_, err := Replace(path, ReplaceInput{Ledger: "race", EntryID: original.EntryID, Title: &title, BasedOn: original.EventID})
					results <- err
					return
				}
				_, err := Delete(path, DeleteInput{Ledger: "race", EntryID: original.EntryID, Reason: label, BasedOn: original.EventID})
				results <- err
			}
			go revise("first")
			<-firstReserved
			go revise("second")
			close(releaseFirst)

			var successes, stale int
			for range 2 {
				err := <-results
				switch {
				case err == nil:
					successes++
				case errors.Is(err, ErrStale):
					stale++
				default:
					t.Fatalf("unstable concurrent error: %v", err)
				}
			}
			if successes != 1 || stale != 1 {
				t.Fatalf("successes=%d stale=%d, want one of each", successes, stale)
			}
			history, err := History(path, "race", original.EntryID)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 2 {
				t.Fatalf("concurrency appended %d events, want exactly one revision", len(history)-1)
			}
		})
	}
}

func TestDatabaseRejectsForksAndCrossEntryChains(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".ledger", "ledger.db")
	first, err := Add(path, AddInput{Ledger: "one", Title: "title", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	insertSuccessor := func(eventID, entryID, ledger, prior string) error {
		_, err := db.Exec(`
			INSERT INTO events (event_id, entry_id, ledger_name, operation, prior_event_id, title, body, created_at)
			VALUES (?, ?, ?, 'replace', ?, 'next', 'body', '2026-01-01T00:00:00Z')`, eventID, entryID, ledger, prior)
		return err
	}
	_, err = db.Exec(`
		INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000009', ?, 'one', 'put', 'other root', 'body', '2026-01-01T00:00:00Z')`, first.EntryID)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate root error=%v, want unique-root rejection", err)
	}
	if err := insertSuccessor("00000000-0000-4000-8000-000000000010", "00000000-0000-4000-8000-000000000011", "one", first.EventID); err == nil || !strings.Contains(err.Error(), "invalid event chain") {
		t.Fatalf("cross-entry chain error=%v", err)
	}
	if err := insertSuccessor("00000000-0000-4000-8000-000000000012", first.EntryID, "two", first.EventID); err == nil || !strings.Contains(err.Error(), "invalid event chain") {
		t.Fatalf("cross-ledger chain error=%v", err)
	}
	if err := insertSuccessor("00000000-0000-4000-8000-000000000013", first.EntryID, "one", first.EventID); err != nil {
		t.Fatalf("valid successor: %v", err)
	}
	if err := insertSuccessor("00000000-0000-4000-8000-000000000014", first.EntryID, "one", first.EventID); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("fork error=%v, want unique-successor rejection", err)
	}
}

func TestVerifyReturnsFindingsEnvelopeForPartialSchemas(t *testing.T) {
	for _, fixture := range []struct {
		name string
		sql  string
	}{
		{"missing events table", `CREATE TABLE unrelated(value TEXT); PRAGMA user_version = 4`},
		{"missing event columns", `CREATE TABLE events(event_seq INTEGER PRIMARY KEY, event_id TEXT); PRAGMA user_version = 4`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(fixture.sql); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := Verify(path)
			if err != nil {
				t.Fatalf("verify aborted instead of returning findings: %v", err)
			}
			if result.OK || len(result.Findings) == 0 || !strings.Contains(strings.ToLower(strings.Join(result.Findings, " ")), "schema") {
				t.Fatalf("partial schema findings=%#v", result)
			}
		})
	}
}

func TestVerifyReportsEveryMalformedEventContractFinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	longTitle := strings.Repeat("t", 1001)
	longAuthor := strings.Repeat("a", 257)
	longSession := strings.Repeat("s", 257)
	_, err = db.Exec(`
		CREATE TABLE events (
			event_seq INTEGER PRIMARY KEY, event_id TEXT, entry_id TEXT, ledger_name TEXT,
			operation TEXT, prior_event_id TEXT, title TEXT, body TEXT, author TEXT, session TEXT,
			deletion_reason TEXT, created_at TEXT, metadata_json TEXT, tags_json TEXT
		);
		INSERT INTO events VALUES
			(1, 'bad-event', 'bad-entry', 'bad name', 'put', NULL, ?, '', ?, ?, 'unexpected', 'not-a-time', '[]', '["ok",7]'),
			(2, '00000000-0000-4000-8000-000000000082', 'bad-entry', 'bad name', 'delete', 'bad-event', 'title', 'body', NULL, NULL, NULL, '2026-01-02T00:00:00Z', '{}', '[]'),
			(3, '00000000-0000-4000-8000-000000000083', '00000000-0000-4000-8000-000000000084', 'valid', 'merge', NULL, 'title', 'body', NULL, NULL, NULL, '2026-01-03T00:00:00Z', '{}', '[]');
		PRAGMA user_version = 4`, longTitle, longAuthor, longSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(path)
	if err != nil {
		t.Fatalf("verify aborted instead of returning findings: %v", err)
	}
	findings := strings.ToLower(strings.Join(result.Findings, "\n"))
	for _, want := range []string{"schema events constraint", "event_id", "entry_id", "ledger", "operation", "title", "body", "author", "session", "deletion_reason", "created_at", "metadata", "tags"} {
		if !strings.Contains(findings, want) {
			t.Fatalf("findings lack %q: %#v", want, result.Findings)
		}
	}
	if result.OK {
		t.Fatalf("malformed rows verified OK: %#v", result)
	}
}

func TestOpenRepairsTamperedProtectionAndIncompatibleFTSSchema(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".ledger", "ledger.db")
	original, err := Add(path, AddInput{Ledger: "test", Title: "kept", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		DROP INDEX events_one_root;
		CREATE INDEX events_one_root ON events(ledger_name, entry_id) WHERE operation = 'put';
		DROP TRIGGER events_append_only_update;
		CREATE TRIGGER events_append_only_update BEFORE UPDATE ON events BEGIN SELECT 1; END;
		DROP TRIGGER events_search_insert;
		DROP TABLE event_search;
		CREATE TABLE event_search(title TEXT)
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	repaired, err := open(path)
	if err != nil {
		t.Fatalf("repair schema: %v", err)
	}
	defer repaired.Close()
	if _, err := repaired.Exec(`
		INSERT INTO events (event_id, entry_id, ledger_name, operation, title, body, created_at)
		VALUES ('00000000-0000-4000-8000-000000000095', ?, 'test', 'put', 'duplicate', 'body', '2026-01-02T00:00:00Z')`, original.EntryID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("tampered root index was not repaired: %v", err)
	}
	if _, err := repaired.Exec(`UPDATE events SET title = title`); err == nil || !strings.Contains(err.Error(), "append-only") {
		var definition string
		_ = repaired.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'events_append_only_update'`).Scan(&definition)
		t.Fatalf("tampered UPDATE trigger was not repaired: %v definition=%q", err, definition)
	}
	var ftsSQL string
	if err := repaired.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'event_search'`).Scan(&ftsSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(ftsSQL), "virtual table") || !strings.Contains(strings.ToLower(ftsSQL), "fts5") {
		t.Fatalf("FTS schema was not rebuilt: %q", ftsSQL)
	}
	results, err := Search(path, "test", "kept", 10, true)
	if err != nil || len(results) != 1 {
		t.Fatalf("rebuilt FTS was not backfilled: results=%#v err=%v", results, err)
	}
}

func TestVerifyFlagsTamperedProtectionDefinition(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".ledger", "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER events_append_only_delete; CREATE TRIGGER events_append_only_delete BEFORE DELETE ON events BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(strings.ToLower(strings.Join(result.Findings, " ")), "events_append_only_delete") {
		t.Fatalf("tampered trigger certified: %#v", result)
	}
}

func TestVerifyFlagsMissingRequiredSchemaProtections(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".ledger", "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX events_one_successor; DROP TRIGGER events_append_only_delete`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	findings := strings.Join(result.Findings, " ")
	if result.OK || !strings.Contains(findings, "events_one_successor") || !strings.Contains(findings, "events_append_only_delete") {
		t.Fatalf("missing protections not reported: %#v", result)
	}
}

func TestFreshSchemaRejectsInvalidMetadataAndTagsAtInsert(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tagTriggerSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'events_tags_are_strings'`).Scan(&tagTriggerSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(tagTriggerSQL), "json_type") || !strings.Contains(strings.ToLower(tagTriggerSQL), "'array'") {
		t.Fatalf("tag trigger lacks explicit array guard: %q", tagTriggerSQL)
	}
	for _, fixture := range []struct {
		name, metadata, tags, errorField string
	}{
		{"metadata non-object", `[]`, `[]`, "metadata"},
		{"tags non-array", `{}`, `{}`, "tags"},
		{"tags mixed-type", `{}`, `["valid",7]`, "tags"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			_, err := db.Exec(`
				INSERT INTO events
					(event_id, entry_id, ledger_name, operation, title, body, created_at, metadata_json, tags_json)
				VALUES
					('00000000-0000-4000-8000-000000000091', '00000000-0000-4000-8000-000000000092',
					 'test', 'put', 'title', 'body', '2026-01-01T00:00:00Z', ?, ?)`, fixture.metadata, fixture.tags)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), fixture.errorField) {
				t.Fatalf("insert error=%v, want %s rejection", err, fixture.errorField)
			}
		})
	}
}

func TestEventReadsRejectNonStringTagValues(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".ledger", "ledger.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		DROP TRIGGER events_tags_are_strings;
		INSERT INTO events
			(event_id, entry_id, ledger_name, operation, title, body, created_at, metadata_json, tags_json)
		VALUES
			('00000000-0000-4000-8000-000000000061', '00000000-0000-4000-8000-000000000062',
			 'test', 'put', 'title', 'body', '2026-01-01T00:00:00Z', '{}', '["valid", 7]')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Show(path, "test", "00000000-0000-4000-8000-000000000062"); err == nil || !strings.Contains(err.Error(), "tags") {
		t.Fatalf("invalid tags error=%v, want decoded-shape rejection", err)
	}
}

func TestOpenEnablesSQLiteSafetySettings(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	db, err := open(filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var foreignKeys, busyTimeout int
	var journalMode string
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || strings.ToLower(journalMode) != "wal" {
		t.Fatalf("foreign_keys=%d busy_timeout=%d journal_mode=%q", foreignKeys, busyTimeout, journalMode)
	}

	_, err = db.Exec(`
		INSERT INTO events
			(event_id, entry_id, ledger_name, operation, prior_event_id, title, body, created_at)
		VALUES
			('00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002',
			 'test', 'replace', '00000000-0000-4000-8000-000000000099', 'title', 'body', '2026-01-01T00:00:00Z')`)
	if err == nil || (!strings.Contains(err.Error(), "FOREIGN KEY") && !strings.Contains(err.Error(), "invalid event chain")) {
		t.Fatalf("missing prior event error = %v, want chain rejection", err)
	}
}
