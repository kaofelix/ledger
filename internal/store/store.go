package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"modernc.org/sqlite"
)

const currentSchemaVersion = 4

const schema = canonicalEventsTableSQL + `;

CREATE UNIQUE INDEX IF NOT EXISTS events_one_successor
    ON events(prior_event_id) WHERE prior_event_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS events_one_root
    ON events(ledger_name, entry_id) WHERE operation = 'put';
CREATE INDEX IF NOT EXISTS events_current_ledger
    ON events(ledger_name, event_seq DESC);

CREATE TRIGGER IF NOT EXISTS events_chain_invariants
BEFORE INSERT ON events
WHEN NEW.prior_event_id IS NOT NULL
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM events prior
        WHERE prior.event_id = NEW.prior_event_id
          AND prior.entry_id = NEW.entry_id
          AND prior.ledger_name = NEW.ledger_name
          AND prior.operation <> 'delete'
    ) THEN RAISE(ABORT, 'invalid event chain') END;
END;

CREATE TRIGGER IF NOT EXISTS events_append_only_update
BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TRIGGER IF NOT EXISTS events_append_only_delete
BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TRIGGER IF NOT EXISTS events_tags_are_strings
BEFORE INSERT ON events
WHEN CASE
    WHEN json_valid(NEW.tags_json) != 1 THEN 1
    WHEN json_type(NEW.tags_json) <> 'array' THEN 1
    WHEN EXISTS (SELECT 1 FROM json_each(NEW.tags_json) WHERE type <> 'text') THEN 1
    ELSE 0
END = 1
BEGIN
    SELECT RAISE(ABORT, 'tags must be an array of strings');
END;

CREATE VIRTUAL TABLE IF NOT EXISTS event_search USING fts5(
    title,
    body,
    deletion_reason,
    event_seq UNINDEXED,
    event_id UNINDEXED,
    entry_id UNINDEXED,
    ledger_name UNINDEXED,
    operation UNINDEXED,
    tokenize = 'unicode61'
);

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
END;

INSERT INTO event_search(
    rowid, title, body, deletion_reason, event_seq,
    event_id, entry_id, ledger_name, operation
)
SELECT
    event_seq, title, body, deletion_reason, event_seq,
    event_id, entry_id, ledger_name, operation
FROM events
WHERE NOT EXISTS (
    SELECT 1 FROM event_search WHERE event_search.rowid = events.event_seq
);
`

// Event is one immutable ledger event.
type Event struct {
	EventID        string         `json:"event_id"`
	EntryID        string         `json:"entry_id"`
	Ledger         string         `json:"ledger"`
	Operation      string         `json:"operation"`
	PriorEventID   string         `json:"prior_event_id,omitempty"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	Author         string         `json:"author,omitempty"`
	Session        string         `json:"session,omitempty"`
	DeletionReason string         `json:"deletion_reason,omitempty"`
	CreatedAt      string         `json:"created_at"`
	Metadata       map[string]any `json:"metadata"`
	Tags           []string       `json:"tags"`
}

var (
	ErrNotFound = errors.New("entry not found")
	ErrDeleted  = errors.New("entry is deleted")
	ErrStale    = errors.New("based-on event is stale")
	ErrConflict = errors.New("concurrent revision conflict")
)

// revisionReservationHook is a deterministic test seam invoked after a revision reserves its write transaction.
var revisionReservationHook = func() {}

// AddInput contains a validated put event.
type AddInput struct {
	Ledger  string
	Title   string
	Body    string
	Author  string
	Session string
}

// ReplaceInput contains validated replacement fields. Nil content inherits current content.
type ReplaceInput struct {
	Ledger  string
	EntryID string
	Title   *string
	Body    *string
	Author  string
	Session string
	BasedOn string
}

// DeleteInput contains a validated tombstone event.
type DeleteInput struct {
	Ledger  string
	EntryID string
	Reason  string
	Author  string
	Session string
	BasedOn string
}

// SearchResult is one ranked immutable event match.
type SearchResult struct {
	Event   Event
	Rank    float64
	Snippet string
	Current bool
}

// LedgerSummary reports current records and all immutable events in a named ledger.
type LedgerSummary struct {
	Name         string `json:"name"`
	CurrentCount int    `json:"current_count"`
	EventCount   int    `json:"event_count"`
}

// Discover finds project storage by walking from cwd toward the root.
func Discover(cwd string) (string, error) {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	for {
		candidate := filepath.Join(dir, ".ledger", "ledger.db")
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect %s: %w", candidate, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not initialized (run 'ledger init' at the project root)")
		}
		dir = parent
	}
}

// Add appends a put event in a transaction.
func Add(path string, input AddInput) (Event, error) {
	db, err := open(path)
	if err != nil {
		return Event{}, err
	}
	defer db.Close()

	entryID, err := newUUID()
	if err != nil {
		return Event{}, fmt.Errorf("generate entry ID: %w", err)
	}
	eventID, err := newUUID()
	if err != nil {
		return Event{}, fmt.Errorf("generate event ID: %w", err)
	}
	event := Event{
		EventID: eventID, EntryID: entryID, Ledger: input.Ledger,
		Operation: "put", Title: input.Title, Body: input.Body,
		Author: input.Author, Session: input.Session,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata:  map[string]any{}, Tags: []string{},
	}

	tx, err := db.Begin()
	if err != nil {
		return Event{}, fmt.Errorf("begin add transaction: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
		INSERT INTO events
			(event_id, entry_id, ledger_name, operation, title, body, author, session, created_at)
		VALUES (?, ?, ?, 'put', ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		event.EventID, event.EntryID, event.Ledger, event.Title, event.Body,
		event.Author, event.Session, event.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("append event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit event: %w", err)
	}
	return event, nil
}

// Delete appends a tombstone linked to the current event.
func Delete(path string, input DeleteInput) (Event, error) {
	db, err := open(path)
	if err != nil {
		return Event{}, revisionError(err)
	}
	defer db.Close()
	conn, err := beginImmediate(db)
	if err != nil {
		return Event{}, fmt.Errorf("begin delete transaction: %w", err)
	}
	defer closeImmediate(conn)
	current, err := currentEvent(conn, input.Ledger, input.EntryID)
	if err != nil {
		return Event{}, err
	}
	revisionReservationHook()
	if input.BasedOn != "" && input.BasedOn != current.EventID {
		return Event{}, fmt.Errorf("%w: current event is %s", ErrStale, current.EventID)
	}
	if current.Operation == "delete" {
		return Event{}, ErrDeleted
	}
	eventID, err := newUUID()
	if err != nil {
		return Event{}, fmt.Errorf("generate event ID: %w", err)
	}
	event := Event{
		EventID: eventID, EntryID: current.EntryID, Ledger: current.Ledger,
		Operation: "delete", PriorEventID: current.EventID,
		Title: current.Title, Body: current.Body, Author: input.Author, Session: input.Session,
		DeletionReason: input.Reason, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: map[string]any{}, Tags: []string{},
	}
	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO events
			(event_id, entry_id, ledger_name, operation, prior_event_id, title, body, author, session, deletion_reason, created_at)
		VALUES (?, ?, ?, 'delete', ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		event.EventID, event.EntryID, event.Ledger, event.PriorEventID, event.Title, event.Body,
		event.Author, event.Session, event.DeletionReason, event.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("append tombstone: %w", revisionError(err))
	}
	if err := commitImmediate(conn); err != nil {
		return Event{}, fmt.Errorf("commit tombstone: %w", err)
	}
	return event, nil
}

// Replace appends a replacement linked to the current event.
func Replace(path string, input ReplaceInput) (Event, error) {
	db, err := open(path)
	if err != nil {
		return Event{}, revisionError(err)
	}
	defer db.Close()

	conn, err := beginImmediate(db)
	if err != nil {
		return Event{}, fmt.Errorf("begin replace transaction: %w", err)
	}
	defer closeImmediate(conn)
	current, err := currentEvent(conn, input.Ledger, input.EntryID)
	if err != nil {
		return Event{}, err
	}
	revisionReservationHook()
	if input.BasedOn != "" && input.BasedOn != current.EventID {
		return Event{}, fmt.Errorf("%w: current event is %s", ErrStale, current.EventID)
	}
	if current.Operation == "delete" {
		return Event{}, ErrDeleted
	}

	eventID, err := newUUID()
	if err != nil {
		return Event{}, fmt.Errorf("generate event ID: %w", err)
	}
	event := Event{
		EventID: eventID, EntryID: current.EntryID, Ledger: current.Ledger,
		Operation: "replace", PriorEventID: current.EventID,
		Title: current.Title, Body: current.Body, Author: input.Author, Session: input.Session,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Metadata:  map[string]any{}, Tags: []string{},
	}
	if input.Title != nil {
		event.Title = *input.Title
	}
	if input.Body != nil {
		event.Body = *input.Body
	}
	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO events
			(event_id, entry_id, ledger_name, operation, prior_event_id, title, body, author, session, created_at)
		VALUES (?, ?, ?, 'replace', ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		event.EventID, event.EntryID, event.Ledger, event.PriorEventID, event.Title, event.Body,
		event.Author, event.Session, event.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("append replacement: %w", revisionError(err))
	}
	if err := commitImmediate(conn); err != nil {
		return Event{}, fmt.Errorf("commit replacement: %w", err)
	}
	return event, nil
}

func beginImmediate(db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, revisionError(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		return nil, revisionError(err)
	}
	return conn, nil
}

func commitImmediate(conn *sql.Conn) error {
	_, err := conn.ExecContext(context.Background(), `COMMIT`)
	return revisionError(err)
}

func closeImmediate(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
	_ = conn.Close()
}

func revisionError(err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		primaryCode := sqliteErr.Code() & 0xff
		if primaryCode == 5 || primaryCode == 6 { // SQLITE_BUSY or SQLITE_LOCKED.
			return fmt.Errorf("%w: retry the revision", ErrConflict)
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "locked") {
		return fmt.Errorf("%w: retry the revision", ErrConflict)
	}
	return err
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func currentEvent(db queryRower, ledgerName, entryID string) (Event, error) {
	var event Event
	err := scanEvent(db.QueryRowContext(context.Background(), `
		SELECT e.event_id, e.entry_id, e.ledger_name, e.operation, e.prior_event_id,
		       e.title, e.body, e.author, e.session, e.deletion_reason, e.created_at,
		       e.metadata_json, e.tags_json
		FROM events e
		WHERE e.ledger_name = ? AND e.entry_id = ?
		  AND NOT EXISTS (SELECT 1 FROM events successor WHERE successor.prior_event_id = e.event_id)`,
		ledgerName, entryID), &event)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("read current entry: %w", err)
	}
	return event, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(scanner rowScanner, event *Event, extra ...any) error {
	var prior, author, session, deletionReason sql.NullString
	var metadataJSON, tagsJSON string
	dest := []any{
		&event.EventID, &event.EntryID, &event.Ledger, &event.Operation, &prior,
		&event.Title, &event.Body, &author, &session, &deletionReason, &event.CreatedAt,
		&metadataJSON, &tagsJSON,
	}
	dest = append(dest, extra...)
	if err := scanner.Scan(dest...); err != nil {
		return err
	}
	event.PriorEventID = prior.String
	event.Author = author.String
	event.Session = session.String
	event.DeletionReason = deletionReason.String
	if err := json.Unmarshal([]byte(metadataJSON), &event.Metadata); err != nil || event.Metadata == nil {
		if err == nil {
			err = errors.New("metadata must be a JSON object")
		}
		return fmt.Errorf("decode event metadata: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &event.Tags); err != nil || event.Tags == nil {
		if err == nil {
			err = errors.New("tags must be a JSON array of strings")
		}
		return fmt.Errorf("decode event tags: %w", err)
	}
	return nil
}

// History returns every event for an entry oldest first.
func History(path, ledgerName, entryID string) ([]Event, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin history transaction: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
		SELECT event_id, entry_id, ledger_name, operation, prior_event_id,
		       title, body, author, session, deletion_reason, created_at,
		       metadata_json, tags_json
		FROM events
		WHERE ledger_name = ? AND entry_id = ?
		ORDER BY event_seq ASC`, ledgerName, entryID)
	if err != nil {
		return nil, fmt.Errorf("query entry history: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := scanEvent(rows, &event); err != nil {
			return nil, fmt.Errorf("read entry history: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read entry history: %w", err)
	}
	if len(events) == 0 {
		return nil, ErrNotFound
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close entry history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit history transaction: %w", err)
	}
	return events, nil
}

// Show returns the current event for an entry, including a tombstone.
func Show(path, ledgerName, entryID string) (Event, error) {
	db, err := open(path)
	if err != nil {
		return Event{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Event{}, fmt.Errorf("begin show transaction: %w", err)
	}
	defer tx.Rollback()
	event, err := currentEvent(tx, ledgerName, entryID)
	if err != nil {
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit show transaction: %w", err)
	}
	return event, nil
}

// Search finds events with plain-text FTS5 matching. By default it returns only current, non-deleted entries.
func Search(path, ledgerName, query string, limit int, history bool) ([]SearchResult, error) {
	match, err := plainTextMatch(query)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("limit must be between 1 and 100")
	}
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	currentFilter := `
		  AND e.operation <> 'delete'
		  AND NOT EXISTS (
		      SELECT 1 FROM events successor WHERE successor.prior_event_id = e.event_id
		  )`
	if history {
		currentFilter = ""
	}
	rows, err := db.Query(`
		SELECT e.event_id, e.entry_id, e.ledger_name, e.operation, e.prior_event_id,
		       e.title, e.body, e.author, e.session, e.deletion_reason, e.created_at,
		       e.metadata_json, e.tags_json,
		       bm25(event_search, 10.0, 1.0, 1.0) AS score,
		       snippet(event_search, -1, '[', ']', ' … ', 24),
		       NOT EXISTS (
		           SELECT 1 FROM events successor WHERE successor.prior_event_id = e.event_id
		       ) AS is_current
		FROM event_search
		JOIN events e ON e.event_seq = event_search.rowid
		WHERE event_search MATCH ? AND e.ledger_name = ?`+currentFilter+`
		ORDER BY score ASC, e.event_seq DESC, e.event_id ASC
		LIMIT ?`, match, ledgerName, limit)
	if err != nil {
		return nil, fmt.Errorf("search events: %w", err)
	}
	defer rows.Close()

	results := make([]SearchResult, 0)
	for rows.Next() {
		var result SearchResult
		if err := scanEvent(rows, &result.Event, &result.Rank, &result.Snippet, &result.Current); err != nil {
			return nil, fmt.Errorf("read search result: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search results: %w", err)
	}
	return results, nil
}

func plainTextMatch(query string) (string, error) {
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
	if len(terms) == 0 {
		return "", fmt.Errorf("query must contain searchable letters or numbers")
	}
	quoted := make([]string, len(terms))
	for i, term := range terms {
		quoted[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " AND "), nil
}

// Export returns immutable events oldest first, optionally filtered to a ledger or current records.
func Export(path, ledgerName string, current bool) ([]Event, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := `
		SELECT e.event_id, e.entry_id, e.ledger_name, e.operation, e.prior_event_id,
		       e.title, e.body, e.author, e.session, e.deletion_reason, e.created_at,
		       e.metadata_json, e.tags_json
		FROM events e
		WHERE 1 = 1`
	args := make([]any, 0, 1)
	if ledgerName != "" {
		query += ` AND e.ledger_name = ?`
		args = append(args, ledgerName)
	}
	if current {
		query += `
		  AND e.operation <> 'delete'
		  AND NOT EXISTS (
		      SELECT 1 FROM events successor WHERE successor.prior_event_id = e.event_id
		  )`
	}
	query += ` ORDER BY e.event_seq ASC`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query export: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		if err := scanEvent(rows, &event); err != nil {
			return nil, fmt.Errorf("read export event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read export events: %w", err)
	}
	return events, nil
}

// Ledgers returns all named ledgers in deterministic name order.
func Ledgers(path string) ([]LedgerSummary, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT e.ledger_name,
		       sum(CASE WHEN e.operation <> 'delete' AND NOT EXISTS (
		           SELECT 1 FROM events successor WHERE successor.prior_event_id = e.event_id
		       ) THEN 1 ELSE 0 END) AS current_count,
		       count(*) AS event_count
		FROM events e
		GROUP BY e.ledger_name
		ORDER BY e.ledger_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("query ledgers: %w", err)
	}
	defer rows.Close()
	summaries := make([]LedgerSummary, 0)
	for rows.Next() {
		var summary LedgerSummary
		if err := rows.Scan(&summary.Name, &summary.CurrentCount, &summary.EventCount); err != nil {
			return nil, fmt.Errorf("read ledger summary: %w", err)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ledger summaries: %w", err)
	}
	return summaries, nil
}

// ListCurrent returns current non-deleted entries newest first.
func ListCurrent(path, ledgerName string) ([]Event, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin list transaction: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
		SELECT e.event_id, e.entry_id, e.ledger_name, e.operation, e.prior_event_id,
		       e.title, e.body, e.author, e.session, e.deletion_reason, e.created_at,
		       e.metadata_json, e.tags_json
		FROM events e
		WHERE e.ledger_name = ?
		  AND e.operation <> 'delete'
		  AND NOT EXISTS (
		      SELECT 1 FROM events successor WHERE successor.prior_event_id = e.event_id
		  )
		ORDER BY e.event_seq DESC`, ledgerName)
	if err != nil {
		return nil, fmt.Errorf("query current entries: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var event Event
		if err := scanEvent(rows, &event); err != nil {
			return nil, fmt.Errorf("read current entry: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read current entries: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close current entries: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit list transaction: %w", err)
	}
	return events, nil
}

func newUUID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

// Init creates or upgrades the project-local database at root.
func Init(root string) error {
	dir := filepath.Join(root, ".ledger")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create storage directory: %w", err)
	}

	path := filepath.Join(dir, "ledger.db")
	db, err := open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set database permissions: %w", err)
	}
	return nil
}

func open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure database: %w", err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	var tableCount int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'events'`).Scan(&tableCount); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	if tableCount == 0 {
		return nil
	}
	if version == currentSchemaVersion {
		matches, err := allSafeSchemaObjectsMatch(db)
		if err != nil {
			return err
		}
		if matches {
			return nil
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`PRAGMA table_info(events)`)
	if err != nil {
		return fmt.Errorf("inspect event columns: %w", err)
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("read event columns: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close event columns: %w", err)
	}
	if !columns["deletion_reason"] {
		if _, err := tx.Exec(`ALTER TABLE events ADD COLUMN deletion_reason TEXT`); err != nil {
			return fmt.Errorf("add deletion reason column: %w", err)
		}
		columns["deletion_reason"] = true
	}
	for _, name := range []string{
		"event_seq", "event_id", "entry_id", "ledger_name", "operation", "prior_event_id",
		"title", "body", "author", "session", "deletion_reason", "created_at", "metadata_json", "tags_json",
	} {
		if !columns[name] {
			return fmt.Errorf("validate events schema: required column %s is missing", name)
		}
	}
	var invalidJSON int
	if err := tx.QueryRow(`
		SELECT count(*) FROM events
		WHERE CASE
			WHEN json_valid(metadata_json) != 1 THEN 1
			WHEN json_type(metadata_json) <> 'object' THEN 1
			WHEN json_valid(tags_json) != 1 THEN 1
			WHEN json_type(tags_json) <> 'array' THEN 1
			WHEN EXISTS (SELECT 1 FROM json_each(tags_json) WHERE type <> 'text') THEN 1
			ELSE 0
		END = 1`).Scan(&invalidJSON); err != nil {
		return fmt.Errorf("validate existing metadata and tags: %w", err)
	}
	if invalidJSON != 0 {
		return fmt.Errorf("validate existing metadata and tags: found %d events with invalid JSON shapes or non-string tags", invalidJSON)
	}
	var invalidCanonicalRows int
	if err := tx.QueryRow(`
		SELECT count(*) FROM events WHERE
			typeof(event_seq) <> 'integer' OR
			typeof(event_id) <> 'text' OR length(event_id) <> 36 OR
			typeof(entry_id) <> 'text' OR length(entry_id) <> 36 OR
			typeof(ledger_name) <> 'text' OR length(ledger_name) NOT BETWEEN 1 AND 64 OR
			typeof(operation) <> 'text' OR operation NOT IN ('put', 'replace', 'delete') OR
			(prior_event_id IS NOT NULL AND typeof(prior_event_id) <> 'text') OR
			typeof(title) <> 'text' OR typeof(body) <> 'text' OR
			(author IS NOT NULL AND typeof(author) <> 'text') OR
			(session IS NOT NULL AND typeof(session) <> 'text') OR
			(deletion_reason IS NOT NULL AND typeof(deletion_reason) <> 'text') OR
			typeof(created_at) <> 'text' OR typeof(metadata_json) <> 'text' OR typeof(tags_json) <> 'text'`).Scan(&invalidCanonicalRows); err != nil {
		return fmt.Errorf("validate legacy rows for canonical events table: %w", err)
	}
	if invalidCanonicalRows != 0 {
		return fmt.Errorf("validate legacy rows for canonical events table: found %d rows that violate v4 types or checks", invalidCanonicalRows)
	}
	var duplicateEventIDs int
	if err := tx.QueryRow(`SELECT count(*) FROM (SELECT event_id FROM events GROUP BY event_id HAVING count(*) > 1)`).Scan(&duplicateEventIDs); err != nil {
		return fmt.Errorf("validate event IDs before canonical rebuild: %w", err)
	}
	if duplicateEventIDs != 0 {
		return fmt.Errorf("validate event IDs before canonical rebuild: found %d duplicate IDs", duplicateEventIDs)
	}
	contractFindings, err := verifyEventContracts(tx)
	if err != nil {
		return fmt.Errorf("validate stable event contracts before canonical rebuild: %w", err)
	}
	if len(contractFindings) != 0 {
		return fmt.Errorf("validate stable event contracts before canonical rebuild: found %d violations; first: %s", len(contractFindings), contractFindings[0])
	}
	var invalidChains int
	if err := tx.QueryRow(`
		SELECT count(*) FROM (
			SELECT e.event_id FROM events e
			LEFT JOIN events p ON p.event_id = e.prior_event_id
			WHERE (e.operation = 'put') <> (e.prior_event_id IS NULL)
			   OR (e.prior_event_id IS NOT NULL AND (
			       p.event_id IS NULL OR p.entry_id <> e.entry_id OR p.ledger_name <> e.ledger_name
			       OR p.operation = 'delete' OR p.event_seq >= e.event_seq))
			UNION ALL
			SELECT e.entry_id FROM events e GROUP BY e.ledger_name, e.entry_id
			HAVING sum(CASE WHEN e.operation = 'put' THEN 1 ELSE 0 END) <> 1
			UNION ALL
			SELECT prior_event_id FROM events WHERE prior_event_id IS NOT NULL
			GROUP BY prior_event_id HAVING count(*) > 1
		)`).Scan(&invalidChains); err != nil {
		return fmt.Errorf("validate existing event chains: %w", err)
	}
	if invalidChains != 0 {
		return fmt.Errorf("validate existing event chains: found %d invariant violations", invalidChains)
	}
	tableCanonical, err := canonicalEventsTableMatches(tx)
	if err != nil {
		return err
	}
	if !tableCanonical {
		if err := rebuildCanonicalEventsTable(tx); err != nil {
			return err
		}
	}
	if err := canonicalizeSafeSchema(tx); err != nil {
		return fmt.Errorf("install current schema protections: %w", err)
	}
	var foreignKeyViolations int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil {
		return fmt.Errorf("validate foreign keys after canonical rebuild: %w", err)
	}
	if foreignKeyViolations != 0 {
		return fmt.Errorf("validate foreign keys after canonical rebuild: found %d violations", foreignKeyViolations)
	}
	if version < currentSchemaVersion {
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
