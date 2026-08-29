package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const MaxSQLQueryBytes = 64 * 1024

// SQLResult is a row-returning query result with database column order preserved.
type SQLResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// Verification is the result of non-mutating database and ledger consistency checks.
type Verification struct {
	OK       bool     `json:"ok"`
	Findings []string `json:"findings"`
}

var (
	pragmaPattern     = regexp.MustCompile(`(?is)^\s*pragma\s+(?:(?:main|temp)\s*\.\s*)?([a-z_]+)\s*(.*)$`)
	storedUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	storedLedgerName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

var safePragmas = map[string]bool{
	"application_id": true, "collation_list": true, "compile_options": true,
	"database_list": true, "encoding": true, "foreign_key_check": true,
	"foreign_key_list": true, "freelist_count": true, "function_list": true,
	"index_info": true, "index_list": true, "index_xinfo": true,
	"integrity_check": true, "journal_mode": true, "module_list": true,
	"page_count": true, "page_size": true, "pragma_list": true,
	"query_only": true, "quick_check": true, "schema_version": true,
	"table_info": true, "table_list": true, "table_xinfo": true,
	"user_version": true,
}

var pragmaArguments = map[string]bool{
	"foreign_key_check": true, "foreign_key_list": true, "index_info": true,
	"index_list": true, "index_xinfo": true, "integrity_check": true,
	"quick_check": true, "table_info": true, "table_xinfo": true,
}

// Verify checks SQLite integrity, foreign keys, event chains, and the derived search index.
func Verify(path string) (Verification, error) {
	result := Verification{OK: true, Findings: make([]string, 0)}
	db, err := openReadOnly(path)
	if err != nil {
		return result, err
	}
	defer db.Close()

	integrityRows, err := db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return result, fmt.Errorf("run SQLite integrity check: %w", err)
	}
	for integrityRows.Next() {
		var finding string
		if err := integrityRows.Scan(&finding); err != nil {
			integrityRows.Close()
			return result, fmt.Errorf("read SQLite integrity check: %w", err)
		}
		if finding != "ok" {
			result.Findings = append(result.Findings, "SQLite integrity: "+finding)
		}
	}
	if err := integrityRows.Close(); err != nil {
		return result, fmt.Errorf("close SQLite integrity check: %w", err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return result, fmt.Errorf("inspect schema version: %w", err)
	}
	if version != currentSchemaVersion {
		result.Findings = append(result.Findings, fmt.Sprintf("schema version: got %d, want %d", version, currentSchemaVersion))
	}
	objects := make(map[string]string)
	definitions := make(map[string]string)
	objectRows, err := db.Query(`SELECT type, name, coalesce(sql, '') FROM sqlite_master`)
	if err != nil {
		return result, fmt.Errorf("inspect schema objects: %w", err)
	}
	for objectRows.Next() {
		var objectType, name, definition string
		if err := objectRows.Scan(&objectType, &name, &definition); err != nil {
			objectRows.Close()
			return result, fmt.Errorf("read schema objects: %w", err)
		}
		objects[name] = objectType
		definitions[name] = definition
	}
	if err := objectRows.Close(); err != nil {
		return result, fmt.Errorf("close schema objects: %w", err)
	}
	if objects["events"] != "table" {
		result.Findings = append(result.Findings, "schema table missing: events")
		result.OK = false
		return result, nil
	}
	columnsCompatible, columnFindings, err := inspectEventTableSchema(db, definitions["events"])
	if err != nil {
		return result, err
	}
	result.Findings = append(result.Findings, columnFindings...)
	for _, spec := range append(append([]schemaObjectSpec{}, protectiveSchemaObjects...), ftsTriggerSpec) {
		if objects[spec.name] != spec.objectType {
			result.Findings = append(result.Findings, fmt.Sprintf("schema protection missing: %s %s", spec.objectType, spec.name))
		} else if normalizedSchemaSQL(definitions[spec.name]) != normalizedSchemaSQL(spec.sql) {
			result.Findings = append(result.Findings, fmt.Sprintf("schema protection definition mismatch: %s", spec.name))
		}
	}
	ftsCompatible := objects[ftsTableSpec.name] == ftsTableSpec.objectType && normalizedSchemaSQL(definitions[ftsTableSpec.name]) == normalizedSchemaSQL(ftsTableSpec.sql)
	if objects[ftsTableSpec.name] != ftsTableSpec.objectType {
		result.Findings = append(result.Findings, "schema protection missing: table event_search")
	} else if !ftsCompatible {
		result.Findings = append(result.Findings, "schema protection definition mismatch: event_search FTS5 configuration")
	}
	if columnsCompatible {
		contractFindings, err := verifyEventContracts(db)
		if err != nil {
			return result, err
		}
		result.Findings = append(result.Findings, contractFindings...)
	}

	foreignRows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		result.Findings = append(result.Findings, fmt.Sprintf("foreign-key check unavailable: %v", err))
	} else {
		for foreignRows.Next() {
			var table, parent string
			var rowID any
			var foreignKey int
			if err := foreignRows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
				result.Findings = append(result.Findings, fmt.Sprintf("foreign-key finding unreadable: %v", err))
				continue
			}
			result.Findings = append(result.Findings, fmt.Sprintf("foreign key: table=%s row=%v parent=%s constraint=%d", table, rowID, parent, foreignKey))
		}
		if err := foreignRows.Close(); err != nil {
			result.Findings = append(result.Findings, fmt.Sprintf("foreign-key check incomplete: %v", err))
		}
	}

	if !columnsCompatible {
		result.OK = len(result.Findings) == 0
		return result, nil
	}

	chainRows, err := db.Query(`
		SELECT finding FROM (
			SELECT e.event_seq AS sequence, 'event chain: root operation/prior mismatch at event ' || e.event_id AS finding
			FROM events e
			WHERE (e.operation = 'put') <> (e.prior_event_id IS NULL)
			UNION ALL
			SELECT e.event_seq, 'event chain: invalid prior at event ' || e.event_id
			FROM events e LEFT JOIN events p ON p.event_id = e.prior_event_id
			WHERE e.prior_event_id IS NOT NULL AND (
				p.event_id IS NULL OR p.entry_id <> e.entry_id OR p.ledger_name <> e.ledger_name
				OR p.operation = 'delete' OR p.event_seq >= e.event_seq
			)
			UNION ALL
			SELECT min(e.event_seq), 'event chain: entry has ' || sum(CASE WHEN e.operation = 'put' THEN 1 ELSE 0 END) || ' roots: ' || e.ledger_name || '/' || e.entry_id
			FROM events e GROUP BY e.ledger_name, e.entry_id
			HAVING sum(CASE WHEN e.operation = 'put' THEN 1 ELSE 0 END) <> 1
			UNION ALL
			SELECT min(e.event_seq), 'event chain: entry has ' || sum(CASE WHEN s.event_id IS NULL THEN 1 ELSE 0 END) || ' current tips: ' || e.ledger_name || '/' || e.entry_id
			FROM events e LEFT JOIN events s ON s.prior_event_id = e.event_id
			GROUP BY e.ledger_name, e.entry_id
			HAVING sum(CASE WHEN s.event_id IS NULL THEN 1 ELSE 0 END) <> 1
			UNION ALL
			SELECT min(e.event_seq), 'event chain: event has multiple successors: ' || e.event_id
			FROM events e JOIN events s ON s.prior_event_id = e.event_id
			GROUP BY e.event_id HAVING count(*) > 1
		) ORDER BY sequence, finding`)
	if err != nil {
		result.Findings = append(result.Findings, fmt.Sprintf("event-chain check unavailable: %v", err))
	} else {
		for chainRows.Next() {
			var finding string
			if err := chainRows.Scan(&finding); err != nil {
				result.Findings = append(result.Findings, fmt.Sprintf("event-chain finding unreadable: %v", err))
				continue
			}
			result.Findings = append(result.Findings, finding)
		}
		if err := chainRows.Close(); err != nil {
			result.Findings = append(result.Findings, fmt.Sprintf("event-chain check incomplete: %v", err))
		}
	}

	var searchMismatch int
	if !ftsCompatible {
		result.OK = len(result.Findings) == 0
		return result, nil
	}
	err = db.QueryRow(`
		SELECT count(*) FROM (
			SELECT e.event_seq
			FROM events e LEFT JOIN event_search s ON s.rowid = e.event_seq
			WHERE s.rowid IS NULL OR s.event_id <> e.event_id OR s.entry_id <> e.entry_id
			   OR s.ledger_name <> e.ledger_name OR s.operation <> e.operation
			   OR s.title <> e.title OR s.body <> e.body
			   OR s.deletion_reason IS NOT e.deletion_reason
			UNION ALL
			SELECT s.rowid FROM event_search s LEFT JOIN events e ON e.event_seq = s.rowid
			WHERE e.event_seq IS NULL
		)`).Scan(&searchMismatch)
	if err != nil {
		result.Findings = append(result.Findings, fmt.Sprintf("search index check unavailable: %v", err))
		result.OK = false
		return result, nil
	}
	if searchMismatch != 0 {
		result.Findings = append(result.Findings, fmt.Sprintf("search index: %d missing, extra, or mismatched rows", searchMismatch))
	}
	result.OK = len(result.Findings) == 0
	return result, nil
}

type inspectedColumn struct {
	columnType   string
	notNull      bool
	primaryKey   int
	defaultValue string
}

func inspectEventTableSchema(db *sql.DB, tableDefinition string) (bool, []string, error) {
	rows, err := db.Query(`PRAGMA table_info(events)`)
	if err != nil {
		return false, nil, fmt.Errorf("inspect events columns: %w", err)
	}
	defer rows.Close()
	columns := make(map[string]inspectedColumn)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, nil, fmt.Errorf("read events columns: %w", err)
		}
		columns[name] = inspectedColumn{strings.ToUpper(columnType), notNull == 1, primaryKey, defaultValue.String}
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("read events columns: %w", err)
	}
	expected := map[string]inspectedColumn{
		"event_seq": {columnType: "INTEGER", primaryKey: 1},
		"event_id":  {columnType: "TEXT", notNull: true}, "entry_id": {columnType: "TEXT", notNull: true},
		"ledger_name": {columnType: "TEXT", notNull: true}, "operation": {columnType: "TEXT", notNull: true},
		"prior_event_id": {columnType: "TEXT"}, "title": {columnType: "TEXT", notNull: true},
		"body": {columnType: "TEXT", notNull: true}, "author": {columnType: "TEXT"},
		"session": {columnType: "TEXT"}, "deletion_reason": {columnType: "TEXT"},
		"created_at":    {columnType: "TEXT", notNull: true},
		"metadata_json": {columnType: "TEXT", notNull: true, defaultValue: "'{}'"},
		"tags_json":     {columnType: "TEXT", notNull: true, defaultValue: "'[]'"},
	}
	findings := make([]string, 0)
	complete := true
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := expected[name]
		got, ok := columns[name]
		if !ok {
			findings = append(findings, fmt.Sprintf("schema column missing: events.%s", name))
			complete = false
			continue
		}
		if got.columnType != want.columnType || got.notNull != want.notNull || got.primaryKey != want.primaryKey || (want.defaultValue != "" && got.defaultValue != want.defaultValue) {
			findings = append(findings, fmt.Sprintf("schema column incompatible: events.%s", name))
		}
	}
	normalized := normalizedSchemaSQL(tableDefinition)
	for _, fragment := range []string{
		"event_seq integer primary key autoincrement",
		"event_id text not null unique check(length(event_id) = 36)",
		"entry_id text not null check(length(entry_id) = 36)",
		"ledger_name text not null check(length(ledger_name) between 1 and 64)",
		"operation text not null check(operation in ('put', 'replace', 'delete'))",
		"prior_event_id text references events(event_id)",
		"check(json_valid(metadata_json) and json_type(metadata_json) = 'object')",
		"check(json_valid(tags_json) and json_type(tags_json) = 'array')",
		"(operation = 'put' and prior_event_id is null) or",
		"(operation in ('replace', 'delete') and prior_event_id is not null)",
		") strict",
	} {
		if !strings.Contains(normalized, fragment) {
			findings = append(findings, "schema events constraint mismatch: "+fragment)
		}
	}
	return complete, findings, nil
}

type eventContractQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func verifyEventContracts(db eventContractQuerier) ([]string, error) {
	rows, err := db.QueryContext(context.Background(), `
		SELECT event_seq, event_id, entry_id, ledger_name, operation, prior_event_id,
		       title, body, author, session, deletion_reason, created_at, metadata_json, tags_json
		FROM events ORDER BY event_seq`)
	if err != nil {
		return nil, fmt.Errorf("query event contracts: %w", err)
	}
	defer rows.Close()
	findings := make([]string, 0)
	for rows.Next() {
		var sequence int64
		var eventID, entryID, ledger, operation, prior, title, body sql.NullString
		var author, session, deletionReason, createdAt, metadataJSON, tagsJSON sql.NullString
		if err := rows.Scan(
			&sequence, &eventID, &entryID, &ledger, &operation, &prior,
			&title, &body, &author, &session, &deletionReason, &createdAt, &metadataJSON, &tagsJSON,
		); err != nil {
			findings = append(findings, fmt.Sprintf("event row %d: cannot decode stable fields: %v", sequence, err))
			continue
		}
		label := fmt.Sprintf("event %d (%s)", sequence, eventID.String)
		add := func(field, detail string) {
			findings = append(findings, fmt.Sprintf("%s: %s %s", label, field, detail))
		}
		if !eventID.Valid || !storedUUIDPattern.MatchString(eventID.String) {
			add("event_id", "must be a UUID")
		}
		if !entryID.Valid || !storedUUIDPattern.MatchString(entryID.String) {
			add("entry_id", "must be a UUID")
		}
		if !ledger.Valid || !storedLedgerName.MatchString(ledger.String) {
			add("ledger", "has an invalid name")
		}
		if prior.Valid && prior.String != "" && !storedUUIDPattern.MatchString(prior.String) {
			add("prior_event_id", "must be a UUID when present")
		}
		if !title.Valid || strings.TrimSpace(title.String) == "" || len(title.String) > 1000 {
			add("title", "must be non-blank and at most 1000 bytes")
		}
		if !body.Valid || strings.TrimSpace(body.String) == "" || len(body.String) > 1024*1024 {
			add("body", "must be non-blank and at most 1 MiB")
		}
		if author.Valid && len(author.String) > 256 {
			add("author", "must be at most 256 bytes")
		}
		if session.Valid && len(session.String) > 256 {
			add("session", "must be at most 256 bytes")
		}
		switch operation.String {
		case "put":
			if prior.Valid && prior.String != "" {
				add("prior_event_id", "must be null for put")
			}
			if deletionReason.Valid && deletionReason.String != "" {
				add("deletion_reason", "must be empty for put")
			}
		case "replace":
			if !prior.Valid || prior.String == "" {
				add("prior_event_id", "is required for replace")
			}
			if deletionReason.Valid && deletionReason.String != "" {
				add("deletion_reason", "must be empty for replace")
			}
		case "delete":
			if !prior.Valid || prior.String == "" {
				add("prior_event_id", "is required for delete")
			}
			if !deletionReason.Valid || strings.TrimSpace(deletionReason.String) == "" || len(deletionReason.String) > 1000 {
				add("deletion_reason", "must be non-blank and at most 1000 bytes for delete")
			}
		default:
			add("operation", "must be put, replace, or delete")
		}
		if !createdAt.Valid {
			add("created_at", "is required")
		} else if _, err := time.Parse(time.RFC3339Nano, createdAt.String); err != nil {
			add("created_at", "must be an RFC3339 timestamp")
		}
		var metadata map[string]any
		if !metadataJSON.Valid || json.Unmarshal([]byte(metadataJSON.String), &metadata) != nil || metadata == nil {
			add("metadata", "must be a JSON object")
		}
		var tags []string
		if !tagsJSON.Valid || json.Unmarshal([]byte(tagsJSON.String), &tags) != nil || tags == nil {
			add("tags", "must be a JSON array of strings")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read event contracts: %w", err)
	}
	return findings, nil
}

// QueryReadOnly executes one row-returning query through a read-only SQLite connection.
func QueryReadOnly(path, query string) (SQLResult, error) {
	if err := validateReadQuery(query); err != nil {
		return SQLResult{}, err
	}
	db, err := openReadOnly(path)
	if err != nil {
		return SQLResult{}, err
	}
	defer db.Close()

	conn, err := db.Conn(context.Background())
	if err != nil {
		return SQLResult{}, fmt.Errorf("open read-only connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA query_only = ON`); err != nil {
		return SQLResult{}, fmt.Errorf("enforce read-only query mode: %w", err)
	}
	rows, err := conn.QueryContext(context.Background(), query)
	if err != nil {
		return SQLResult{}, fmt.Errorf("read-only query rejected: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return SQLResult{}, fmt.Errorf("read query columns: %w", err)
	}
	if len(columns) == 0 {
		return SQLResult{}, fmt.Errorf("query must return rows")
	}
	result := SQLResult{Columns: columns, Rows: make([][]any, 0)}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return SQLResult{}, fmt.Errorf("read query row: %w", err)
		}
		for i, value := range values {
			if blob, ok := value.([]byte); ok {
				values[i] = append([]byte(nil), blob...)
			}
		}
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return SQLResult{}, fmt.Errorf("read query rows: %w", err)
	}
	return result, nil
}

func openReadOnly(path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	return db, nil
}

func validateReadQuery(query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("query must be non-blank")
	}
	if len(query) > MaxSQLQueryBytes {
		return fmt.Errorf("query must be at most %d bytes", MaxSQLQueryBytes)
	}
	clean, statements, err := inspectSQL(query)
	if err != nil {
		return err
	}
	if statements != 1 {
		return fmt.Errorf("exactly one SQL statement is required")
	}
	fields := strings.Fields(clean)
	if len(fields) == 0 {
		return fmt.Errorf("query must be non-blank")
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "WITH":
		return nil
	case "PRAGMA":
		matches := pragmaPattern.FindStringSubmatch(clean)
		if matches == nil || !safePragmas[strings.ToLower(matches[1])] {
			return fmt.Errorf("unsafe or unsupported PRAGMA")
		}
		tail := strings.TrimSpace(strings.TrimSuffix(matches[2], ";"))
		if tail == "" {
			return nil
		}
		if strings.Contains(tail, "=") || !pragmaArguments[strings.ToLower(matches[1])] || !strings.HasPrefix(tail, "(") || !strings.HasSuffix(tail, ")") {
			return fmt.Errorf("unsafe or unsupported PRAGMA assignment")
		}
		return nil
	default:
		return fmt.Errorf("only row-returning SELECT, CTE, and safe PRAGMA queries are allowed")
	}
}

// inspectSQL removes comments and quoted contents for statement counting and keyword checks.
func inspectSQL(query string) (string, int, error) {
	var clean strings.Builder
	statements := 0
	hasCode := false
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"', '`':
			quote := query[i]
			clean.WriteByte(quote)
			hasCode = true
			i++
			closed := false
			for i < len(query) {
				if query[i] == quote {
					if i+1 < len(query) && query[i+1] == quote {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "", 0, fmt.Errorf("unterminated SQL quote")
			}
			clean.WriteByte(quote)
		case '[':
			clean.WriteByte('[')
			hasCode = true
			end := strings.IndexByte(query[i+1:], ']')
			if end < 0 {
				return "", 0, fmt.Errorf("unterminated SQL identifier")
			}
			i += end + 2
			clean.WriteByte(']')
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				i += 2
				for i < len(query) && query[i] != '\n' {
					i++
				}
				clean.WriteByte(' ')
				continue
			}
			clean.WriteByte(query[i])
			hasCode = true
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				end := strings.Index(query[i+2:], "*/")
				if end < 0 {
					return "", 0, fmt.Errorf("unterminated SQL comment")
				}
				i += end + 4
				clean.WriteByte(' ')
				continue
			}
			clean.WriteByte(query[i])
			hasCode = true
			i++
		case ';':
			if hasCode {
				statements++
				hasCode = false
			}
			clean.WriteByte(';')
			i++
		default:
			clean.WriteByte(query[i])
			if query[i] != ' ' && query[i] != '\t' && query[i] != '\r' && query[i] != '\n' {
				hasCode = true
			}
			i++
		}
	}
	if hasCode {
		statements++
	}
	return clean.String(), statements, nil
}
