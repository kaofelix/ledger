package app_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"ledger/internal/app"
)

func TestAddFromSubdirectoryPersistsPutAndPrintsStableEntryID(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	child := filepath.Join(root, "docs", "drafts")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	stdout := runOK(t, child, "add", "decisions", "--title", "Choose SQLite", "--body", "Keep project data local", "--author", "Ada", "--session", "planning-7")
	entryID := strings.TrimSpace(stdout)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(entryID) {
		t.Fatalf("stdout is not a UUID entry ID: %q", stdout)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotEntryID, operation, title, body, author, session string
	err = db.QueryRow(`SELECT entry_id, operation, title, body, author, session FROM events`).Scan(&gotEntryID, &operation, &title, &body, &author, &session)
	if err != nil {
		t.Fatalf("read persisted event: %v", err)
	}
	if gotEntryID != entryID || operation != "put" || title != "Choose SQLite" || body != "Keep project data local" || author != "Ada" || session != "planning-7" {
		t.Fatalf("unexpected event: entry=%q operation=%q title=%q body=%q author=%q session=%q", gotEntryID, operation, title, body, author, session)
	}
}

func TestReplaceAppendsEventAndInheritsOmittedContent(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	entryID := strings.TrimSpace(runOK(t, root, "add", "decisions", "--title", "Original", "--body", "Keep this body"))

	replacedJSON := runOK(t, root, "replace", "decisions", entryID, "--title", "Revised", "--author", "Ada", "--json")
	var replaced map[string]any
	if err := json.Unmarshal([]byte(replacedJSON), &replaced); err != nil {
		t.Fatalf("replace output is not JSON: %v; output=%q", err, replacedJSON)
	}
	if replaced["operation"] != "replace" || replaced["entry_id"] != entryID || replaced["title"] != "Revised" || replaced["body"] != "Keep this body" || replaced["prior_event_id"] == nil {
		t.Fatalf("unexpected replacement: %#v", replaced)
	}

	listedJSON := runOK(t, root, "list", "decisions", "--json")
	var listed []map[string]any
	if err := json.Unmarshal([]byte(listedJSON), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0]["event_id"] != replaced["event_id"] {
		t.Fatalf("list did not return only replacement: %#v", listed)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE entry_id = ?`, entryID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("history count=%d err=%v, want 2 immutable events", count, err)
	}
}

func TestShowReturnsOnlyCurrentEntryState(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	addedJSON := runOK(t, root, "add", "decisions", "--title", "Original", "--body", "Original body", "--json")
	var added map[string]any
	if err := json.Unmarshal([]byte(addedJSON), &added); err != nil {
		t.Fatal(err)
	}
	entryID := added["entry_id"].(string)
	replacedJSON := runOK(t, root, "replace", "decisions", entryID, "--body", "Current body", "--json")
	var replaced map[string]any
	if err := json.Unmarshal([]byte(replacedJSON), &replaced); err != nil {
		t.Fatal(err)
	}

	shownJSON := runOK(t, root, "show", "decisions", entryID, "--json")
	var shown map[string]any
	if err := json.Unmarshal([]byte(shownJSON), &shown); err != nil {
		t.Fatalf("show output is not JSON: %v; output=%q", err, shownJSON)
	}
	if shown["event_id"] != replaced["event_id"] || shown["title"] != "Original" || shown["body"] != "Current body" {
		t.Fatalf("show did not return current state: %#v", shown)
	}
}

func TestDeleteAppendsVisibleTombstoneAndHidesEntryFromList(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	entryID := strings.TrimSpace(runOK(t, root, "add", "notes", "--title", "Temporary", "--body", "Remove later"))

	deletedJSON := runOK(t, root, "delete", "notes", entryID, "--reason", "No longer relevant", "--author", "Ada", "--json")
	var deleted map[string]any
	if err := json.Unmarshal([]byte(deletedJSON), &deleted); err != nil {
		t.Fatalf("delete output is not JSON: %v; output=%q", err, deletedJSON)
	}
	if deleted["operation"] != "delete" || deleted["deletion_reason"] != "No longer relevant" || deleted["prior_event_id"] == nil {
		t.Fatalf("unexpected tombstone: %#v", deleted)
	}

	shown := runOK(t, root, "show", "notes", entryID, "--json")
	var current map[string]any
	if err := json.Unmarshal([]byte(shown), &current); err != nil {
		t.Fatal(err)
	}
	if current["operation"] != "delete" || current["event_id"] != deleted["event_id"] || current["deletion_reason"] != "No longer relevant" {
		t.Fatalf("show did not clearly return tombstone: %#v", current)
	}
	human := runOK(t, root, "show", "notes", entryID)
	if !strings.Contains(human, "Status: deleted") || !strings.Contains(human, "Reason: No longer relevant") || !strings.Contains(human, deleted["event_id"].(string)) {
		t.Fatalf("human show did not clearly identify tombstone: %q", human)
	}
	if listed := runOK(t, root, "list", "notes", "--json"); strings.TrimSpace(listed) != "[]" {
		t.Fatalf("deleted entry remained in list: %q", listed)
	}
}

func TestHistoryReturnsEveryEventOldestFirstIncludingReason(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	addedJSON := runOK(t, root, "add", "notes", "--title", "One", "--body", "First", "--json")
	var added map[string]any
	if err := json.Unmarshal([]byte(addedJSON), &added); err != nil {
		t.Fatal(err)
	}
	entryID := added["entry_id"].(string)
	replacedJSON := runOK(t, root, "replace", "notes", entryID, "--title", "Two", "--json")
	var replaced map[string]any
	if err := json.Unmarshal([]byte(replacedJSON), &replaced); err != nil {
		t.Fatal(err)
	}
	deletedJSON := runOK(t, root, "delete", "notes", entryID, "--reason", "Superseded", "--json")
	var deleted map[string]any
	if err := json.Unmarshal([]byte(deletedJSON), &deleted); err != nil {
		t.Fatal(err)
	}

	historyJSON := runOK(t, root, "history", "notes", entryID, "--json")
	var history []map[string]any
	if err := json.Unmarshal([]byte(historyJSON), &history); err != nil {
		t.Fatalf("history output is not JSON: %v; output=%q", err, historyJSON)
	}
	if len(history) != 3 || history[0]["event_id"] != added["event_id"] || history[1]["event_id"] != replaced["event_id"] || history[2]["event_id"] != deleted["event_id"] {
		t.Fatalf("history is not complete and oldest-first: %#v", history)
	}
	if history[0]["prior_event_id"] != nil || history[1]["prior_event_id"] != added["event_id"] || history[2]["deletion_reason"] != "Superseded" {
		t.Fatalf("history links or reason missing: %#v", history)
	}
	human := runOK(t, root, "history", "notes", entryID)
	if strings.Index(human, "One") > strings.Index(human, "Two") || !strings.Contains(human, "Reason: Superseded") {
		t.Fatalf("human history is incomplete or out of order: %q", human)
	}
}

func TestBasedOnRejectsStaleRevisionWithoutAppending(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	addedJSON := runOK(t, root, "add", "decisions", "--title", "One", "--body", "Body", "--json")
	var added map[string]any
	if err := json.Unmarshal([]byte(addedJSON), &added); err != nil {
		t.Fatal(err)
	}
	entryID := added["entry_id"].(string)
	originalEventID := added["event_id"].(string)
	runOK(t, root, "replace", "decisions", entryID, "--title", "Two", "--based-on", originalEventID)

	var stdout, stderr bytes.Buffer
	code := app.Run([]string{"delete", "decisions", entryID, "--reason", "stale attempt", "--based-on", originalEventID}, root, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "stale") {
		t.Fatalf("code=%d stderr=%q, want stale rejection", code, stderr.String())
	}

	historyJSON := runOK(t, root, "history", "decisions", entryID, "--json")
	var history []map[string]any
	if err := json.Unmarshal([]byte(historyJSON), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1]["operation"] != "replace" {
		t.Fatalf("stale delete appended or changed current state: %#v", history)
	}
}

func TestDeletedEntryRejectsFurtherRevision(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	entryID := strings.TrimSpace(runOK(t, root, "add", "notes", "--title", "One", "--body", "Body"))
	runOK(t, root, "delete", "notes", entryID, "--reason", "done")

	var stdout, stderr bytes.Buffer
	code := app.Run([]string{"replace", "notes", entryID, "--title", "Resurrected"}, root, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "deleted") {
		t.Fatalf("code=%d stderr=%q, want deleted rejection", code, stderr.String())
	}
}

func TestLedgersListsDeterministicCurrentAndImmutableCounts(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	zetaID := strings.TrimSpace(runOK(t, root, "add", "zeta", "--title", "Z", "--body", "first"))
	runOK(t, root, "replace", "zeta", zetaID, "--body", "second")
	deletedID := strings.TrimSpace(runOK(t, root, "add", "alpha", "--title", "A", "--body", "remove"))
	runOK(t, root, "delete", "alpha", deletedID, "--reason", "done")
	runOK(t, root, "add", "alpha", "--title", "Kept", "--body", "current")

	var ledgers []struct {
		Name         string `json:"name"`
		CurrentCount int    `json:"current_count"`
		EventCount   int    `json:"event_count"`
	}
	output := runOK(t, root, "ledgers", "--json")
	if err := json.Unmarshal([]byte(output), &ledgers); err != nil {
		t.Fatalf("ledgers output is not JSON: %v; output=%q", err, output)
	}
	if len(ledgers) != 2 || ledgers[0].Name != "alpha" || ledgers[0].CurrentCount != 1 || ledgers[0].EventCount != 3 || ledgers[1].Name != "zeta" || ledgers[1].CurrentCount != 1 || ledgers[1].EventCount != 2 {
		t.Fatalf("unexpected ledger summary: %#v", ledgers)
	}
	human := runOK(t, root, "ledgers")
	if strings.Index(human, "alpha") > strings.Index(human, "zeta") || !strings.Contains(human, "CURRENT\tEVENTS") {
		t.Fatalf("human ledgers output is not a deterministic table: %q", human)
	}
}

func TestListShowsOnlyNamedLedgerNewestFirst(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	runOK(t, root, "add", "decisions", "--title", "First", "--body", "older body")
	runOK(t, root, "add", "notes", "--title", "Hidden", "--body", "other ledger")
	runOK(t, root, "add", "decisions", "--title", "Second", "--body", "newer body")

	output := runOK(t, root, "list", "decisions")
	newer := strings.Index(output, "Second")
	older := strings.Index(output, "First")
	if newer < 0 || older < 0 || newer >= older {
		t.Fatalf("entries are not newest first: %q", output)
	}
	if strings.Contains(output, "Hidden") {
		t.Fatalf("list included another named ledger: %q", output)
	}
}

func TestEventReadsEmitStoredMetadataAndTags(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO events
			(event_id, entry_id, ledger_name, operation, title, body, created_at, metadata_json, tags_json)
		VALUES
			('00000000-0000-4000-8000-000000000041', '00000000-0000-4000-8000-000000000042',
			 'facts', 'put', 'Stored JSON', 'Real values', '2026-01-01T00:00:00Z',
			 '{"confidence":0.75,"source":"operator"}', '["sqlite","local"]')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	output := runOK(t, root, "show", "facts", "00000000-0000-4000-8000-000000000042", "--json")
	var event map[string]any
	if err := json.Unmarshal([]byte(output), &event); err != nil {
		t.Fatal(err)
	}
	metadata, ok := event["metadata"].(map[string]any)
	if !ok || metadata["source"] != "operator" || metadata["confidence"] != 0.75 {
		t.Fatalf("stored metadata not emitted: %#v", event["metadata"])
	}
	tags, ok := event["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "sqlite" || tags[1] != "local" {
		t.Fatalf("stored tags not emitted: %#v", event["tags"])
	}
}

func TestJSONAddAndListProvideMachineReadableEvents(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	addedJSON := runOK(t, root, "add", "ideas", "--title", "Machine output", "--body", "Parse this", "--json")

	var added map[string]any
	if err := json.Unmarshal([]byte(addedJSON), &added); err != nil {
		t.Fatalf("add output is not JSON: %v; output=%q", err, addedJSON)
	}
	if added["entry_id"] == "" || added["event_id"] == "" || added["ledger"] != "ideas" || added["operation"] != "put" {
		t.Fatalf("unexpected add JSON: %#v", added)
	}

	listedJSON := runOK(t, root, "list", "ideas", "--json")
	var listed []map[string]any
	if err := json.Unmarshal([]byte(listedJSON), &listed); err != nil {
		t.Fatalf("list output is not a JSON array: %v; output=%q", err, listedJSON)
	}
	if len(listed) != 1 || listed[0]["entry_id"] != added["entry_id"] || listed[0]["title"] != "Machine output" || listed[0]["body"] != "Parse this" {
		t.Fatalf("unexpected list JSON: %#v", listed)
	}
	if _, ok := listed[0]["metadata"]; !ok {
		t.Fatalf("list JSON lacks metadata placeholder: %#v", listed[0])
	}
	if _, ok := listed[0]["tags"]; !ok {
		t.Fatalf("list JSON lacks tags placeholder: %#v", listed[0])
	}
}

func TestSearchReturnsRankedCurrentMatchesWithContext(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	strongJSON := runOK(t, root, "add", "notes", "--title", "Quartz decision", "--body", "Chosen storage", "--json")
	var strong map[string]any
	if err := json.Unmarshal([]byte(strongJSON), &strong); err != nil {
		t.Fatal(err)
	}
	runOK(t, root, "add", "notes", "--title", "Storage decision", "--body", "Quartz appears in supporting detail")
	supersededID := strings.TrimSpace(runOK(t, root, "add", "notes", "--title", "Quartz obsolete", "--body", "Old approach"))
	runOK(t, root, "replace", "notes", supersededID, "--title", "Current replacement", "--body", "No matching term")
	deletedID := strings.TrimSpace(runOK(t, root, "add", "notes", "--title", "Quartz discarded", "--body", "Temporary"))
	runOK(t, root, "delete", "notes", deletedID, "--reason", "remove it")
	runOK(t, root, "add", "other", "--title", "Quartz elsewhere", "--body", "Wrong ledger")

	searchJSON := runOK(t, root, "search", "notes", "quartz", "--json")
	var results []map[string]any
	if err := json.Unmarshal([]byte(searchJSON), &results); err != nil {
		t.Fatalf("search output is not JSON: %v; output=%q", err, searchJSON)
	}
	if len(results) != 2 {
		t.Fatalf("results=%#v, want only two current non-deleted matches", results)
	}
	if results[0]["event_id"] != strong["event_id"] {
		t.Fatalf("title match was not ranked first: %#v", results)
	}
	for _, result := range results {
		if result["operation"] == "delete" || result["current"] != true {
			t.Fatalf("default result is deleted or non-current: %#v", result)
		}
		if _, ok := result["rank"].(float64); !ok {
			t.Fatalf("result lacks numeric rank: %#v", result)
		}
		if snippet, ok := result["snippet"].(string); !ok || !strings.Contains(strings.ToLower(snippet), "quartz") {
			t.Fatalf("result lacks match context: %#v", result)
		}
		for _, field := range []string{"event_id", "entry_id", "ledger", "operation", "prior_event_id", "title", "body", "author", "session", "deletion_reason", "created_at", "metadata", "tags"} {
			if _, ok := result[field]; !ok {
				t.Fatalf("result lacks stable field %q: %#v", field, result)
			}
		}
	}
}

func TestSearchHumanOutputIncludesFullEventAndMatchDetails(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	addedJSON := runOK(t, root, "add", "notes", "--title", "Quartz title", "--body", "Full authoritative body", "--author", "Ada", "--json")
	var added map[string]any
	if err := json.Unmarshal([]byte(addedJSON), &added); err != nil {
		t.Fatal(err)
	}

	output := runOK(t, root, "search", "notes", "quartz")
	for _, want := range []string{
		"Ledger: notes",
		"Event: " + added["event_id"].(string),
		"Full authoritative body",
		"Current: true",
		"Rank:",
		"Match:",
		"Author: Ada",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("human search lacks %q: %q", want, output)
		}
	}
}

func TestSearchHistoryIncludesSupersededEventsAndMatchingTombstones(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	entryID := strings.TrimSpace(runOK(t, root, "add", "notes", "--title", "Ancestral wording", "--body", "First revision"))
	runOK(t, root, "replace", "notes", entryID, "--title", "Current wording", "--body", "Second revision")
	runOK(t, root, "delete", "notes", entryID, "--reason", "Retiredbecause duplicate")

	oldJSON := runOK(t, root, "search", "notes", "ancestral", "--history", "--json")
	var oldResults []map[string]any
	if err := json.Unmarshal([]byte(oldJSON), &oldResults); err != nil {
		t.Fatal(err)
	}
	if len(oldResults) != 1 || oldResults[0]["operation"] != "put" || oldResults[0]["current"] != false {
		t.Fatalf("superseded revision status is unclear: %#v", oldResults)
	}

	deletedJSON := runOK(t, root, "search", "notes", "retiredbecause", "--history", "--json")
	var deletedResults []map[string]any
	if err := json.Unmarshal([]byte(deletedJSON), &deletedResults); err != nil {
		t.Fatal(err)
	}
	if len(deletedResults) != 1 || deletedResults[0]["operation"] != "delete" || deletedResults[0]["current"] != true || deletedResults[0]["deletion_reason"] != "Retiredbecause duplicate" {
		t.Fatalf("matching tombstone status or reason is unclear: %#v", deletedResults)
	}
	if !strings.Contains(strings.ToLower(deletedResults[0]["snippet"].(string)), "retiredbecause") {
		t.Fatalf("tombstone reason lacks match context: %#v", deletedResults[0])
	}
}

func TestSearchUsesStableTieBreakingAndBoundedLimit(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	firstJSON := runOK(t, root, "add", "notes", "--title", "Same nebula", "--body", "Identical text", "--json")
	secondJSON := runOK(t, root, "add", "notes", "--title", "Same nebula", "--body", "Identical text", "--json")
	var first, second map[string]any
	if err := json.Unmarshal([]byte(firstJSON), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(secondJSON), &second); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		limitedJSON := runOK(t, root, "search", "notes", "nebula", "--limit", "1", "--json")
		var results []map[string]any
		if err := json.Unmarshal([]byte(limitedJSON), &results); err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0]["event_id"] != second["event_id"] || results[0]["event_id"] == first["event_id"] {
			t.Fatalf("attempt %d unstable limited results: %#v", attempt+1, results)
		}
	}
}

func TestContextJSONReturnsOnlyRetrievedCurrentEntriesInStableEnvelope(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	currentJSON := runOK(t, root, "add", "decisions", "--title", "Quartz storage", "--body", "Current project record", "--json")
	var current map[string]any
	if err := json.Unmarshal([]byte(currentJSON), &current); err != nil {
		t.Fatal(err)
	}
	supersededID := strings.TrimSpace(runOK(t, root, "add", "decisions", "--title", "Quartz old", "--body", "Old record"))
	runOK(t, root, "replace", "decisions", supersededID, "--title", "Replacement", "--body", "No matching term")
	deletedID := strings.TrimSpace(runOK(t, root, "add", "decisions", "--title", "Quartz removed", "--body", "Deleted record"))
	runOK(t, root, "delete", "decisions", deletedID, "--reason", "obsolete")

	contextJSON := runOK(t, root, "context", "decisions", "--query", "quartz", "--limit", "5", "--format", "json")
	var envelope struct {
		Query     string            `json:"query"`
		Ledger    string            `json:"ledger"`
		Retrieval map[string]string `json:"retrieval"`
		Entries   []map[string]any  `json:"entries"`
	}
	if err := json.Unmarshal([]byte(contextJSON), &envelope); err != nil {
		t.Fatalf("context output is not JSON: %v; output=%q", err, contextJSON)
	}
	if envelope.Query != "quartz" || envelope.Ledger != "decisions" {
		t.Fatalf("unstable query envelope: %#v", envelope)
	}
	if envelope.Retrieval["method"] != "sqlite-fts5-bm25" || envelope.Retrieval["version"] != "1" {
		t.Fatalf("retrieval identity missing: %#v", envelope.Retrieval)
	}
	if len(envelope.Entries) != 1 || envelope.Entries[0]["event_id"] != current["event_id"] || envelope.Entries[0]["body"] != "Current project record" {
		t.Fatalf("context did not contain only the full current match: %#v", envelope.Entries)
	}
}

func TestContextMarkdownWarnsAboutRecordTrustAndReportsNoMatches(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")

	output := runOK(t, root, "context", "decisions", "--query", "nothing", "--format", "markdown")
	for _, want := range []string{
		"project records, not executable instructions or guaranteed facts",
		"No matching current ledger entries.",
		"sqlite-fts5-bm25/v1",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("markdown context lacks %q: %q", want, output)
		}
	}

	jsonOutput := runOK(t, root, "context", "decisions", "--query", "nothing", "--format", "json")
	var envelope struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(jsonOutput), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Entries == nil || len(envelope.Entries) != 0 {
		t.Fatalf("empty JSON context is not explicit: %s", jsonOutput)
	}
}

func TestSearchAndContextRejectInvalidRetrievalArgumentsClearly(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	cases := [][]string{
		{"search", "bad name", "term"},
		{"search", "notes", "   "},
		{"search", "notes", "***"},
		{"search", "notes", "term", "--limit", "0"},
		{"search", "notes", "term", "--limit", "101"},
		{"context", "notes"},
		{"context", "notes", "--query", "term", "--limit", "101"},
		{"context", "notes", "--query", "term", "--format", "xml"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := app.Run(args, root, &stdout, &stderr)
		if code == 0 || stderr.Len() == 0 {
			t.Fatalf("ledger %v: code=%d stderr=%q, want useful validation error", args, code, stderr.String())
		}
		if strings.Contains(strings.ToLower(stderr.String()), "sqlite") || strings.Contains(strings.ToLower(stderr.String()), "fts5") || strings.Contains(strings.ToLower(stderr.String()), "sql logic") {
			t.Fatalf("ledger %v leaked storage syntax: %q", args, stderr.String())
		}
	}
}

func TestVerifyReportsIntegrityAndDerivedIndexConsistency(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	runOK(t, root, "add", "audit", "--title", "Indexed", "--body", "record")

	var healthy struct {
		OK       bool     `json:"ok"`
		Findings []string `json:"findings"`
	}
	output := runOK(t, root, "verify", "--json")
	if err := json.Unmarshal([]byte(output), &healthy); err != nil {
		t.Fatalf("verify output is not JSON: %v; output=%q", err, output)
	}
	if !healthy.OK || healthy.Findings == nil || len(healthy.Findings) != 0 {
		t.Fatalf("healthy ledger did not verify: %#v", healthy)
	}
	if human := runOK(t, root, "verify"); !strings.Contains(human, "OK") {
		t.Fatalf("human verification lacks success: %q", human)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM event_search`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := app.Run([]string{"verify", "--json"}, root, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("verify accepted inconsistent index: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var broken struct {
		OK       bool     `json:"ok"`
		Findings []string `json:"findings"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &broken); err != nil || broken.OK || len(broken.Findings) == 0 || !strings.Contains(strings.ToLower(strings.Join(broken.Findings, " ")), "search index") {
		t.Fatalf("index finding is unclear: err=%v result=%#v stderr=%q", err, broken, stderr.String())
	}
}

func TestExportWritesOldestFirstImmutableJSONLAndCurrentFiltering(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO events
			(event_id, entry_id, ledger_name, operation, title, body, created_at, metadata_json, tags_json)
		VALUES
			('00000000-0000-4000-8000-000000000051', '00000000-0000-4000-8000-000000000052',
			 'alpha', 'put', 'First', 'original', '2026-01-01T00:00:00Z',
			 '{"source":"import"}', '["seed"]')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	replacementJSON := runOK(t, root, "replace", "alpha", "00000000-0000-4000-8000-000000000052", "--body", "current", "--json")
	var replacement map[string]any
	if err := json.Unmarshal([]byte(replacementJSON), &replacement); err != nil {
		t.Fatal(err)
	}
	deletedID := strings.TrimSpace(runOK(t, root, "add", "beta", "--title", "Delete", "--body", "gone"))
	runOK(t, root, "delete", "beta", deletedID, "--reason", "done")
	currentJSON := runOK(t, root, "add", "zeta", "--title", "Current", "--body", "kept", "--json")
	var current map[string]any
	if err := json.Unmarshal([]byte(currentJSON), &current); err != nil {
		t.Fatal(err)
	}

	all := decodeJSONLines(t, runOK(t, root, "export"))
	if len(all) != 5 || all[0]["event_id"] != "00000000-0000-4000-8000-000000000051" || all[1]["event_id"] != replacement["event_id"] || all[4]["event_id"] != current["event_id"] {
		t.Fatalf("complete export is not immutable oldest-first stream: %#v", all)
	}
	metadata, _ := all[0]["metadata"].(map[string]any)
	tags, _ := all[0]["tags"].([]any)
	if metadata["source"] != "import" || len(tags) != 1 || tags[0] != "seed" {
		t.Fatalf("export replaced stored metadata or tags: %#v", all[0])
	}
	alpha := decodeJSONLines(t, runOK(t, root, "export", "alpha", "--format", "jsonl"))
	if len(alpha) != 2 || alpha[0]["ledger"] != "alpha" || alpha[1]["event_id"] != replacement["event_id"] {
		t.Fatalf("ledger export is incorrect: %#v", alpha)
	}
	currentOnly := decodeJSONLines(t, runOK(t, root, "export", "--current"))
	if len(currentOnly) != 2 || currentOnly[0]["event_id"] != replacement["event_id"] || currentOnly[1]["event_id"] != current["event_id"] {
		t.Fatalf("current export included deleted or historical records: %#v", currentOnly)
	}

	emptyRoot := t.TempDir()
	runOK(t, emptyRoot, "init")
	if output := runOK(t, emptyRoot, "export"); output != "" {
		t.Fatalf("empty export wrote content: %q", output)
	}
}

func decodeJSONLines(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if output == "" {
		return nil
	}
	result := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		result = append(result, event)
	}
	return result
}

func TestSQLReturnsOrderedColumnsAndSensibleJSONRows(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	runOK(t, root, "add", "facts", "--title", "One", "--body", "Body")

	output := runOK(t, root, "sql", `SELECT ledger_name AS ledger, count(*) AS n, NULL AS absent FROM events GROUP BY ledger_name`, "--json")
	var result struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("sql output is not JSON: %v; output=%q", err, output)
	}
	if strings.Join(result.Columns, ",") != "ledger,n,absent" || len(result.Rows) != 1 || result.Rows[0][0] != "facts" || result.Rows[0][1] != float64(1) || result.Rows[0][2] != nil {
		t.Fatalf("unstable SQL result: %#v", result)
	}
	human := runOK(t, root, "sql", `WITH values_cte(v) AS (VALUES ('x')) SELECT v FROM values_cte`)
	if human != "v\nx\n" {
		t.Fatalf("unexpected human SQL table: %q", human)
	}
}

func TestSQLMutationAttemptsCannotChangeDataOrSchema(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	runOK(t, root, "add", "audit", "--title", "Original", "--body", "Must remain")

	attempts := []string{
		`UPDATE events SET title = 'Changed'`,
		`DELETE FROM events`,
		`DROP TABLE events`,
		`CREATE TABLE injected(value TEXT)`,
		`WITH chosen AS (SELECT event_id FROM events) UPDATE events SET title = 'Changed' WHERE event_id IN chosen`,
		`PRAGMA user_version = 99`,
		`SELECT count(*) FROM events; DELETE FROM events`,
	}
	for _, query := range attempts {
		var stdout, stderr bytes.Buffer
		if code := app.Run([]string{"sql", query, "--json"}, root, &stdout, &stderr); code == 0 || stderr.Len() == 0 {
			t.Fatalf("unsafe SQL succeeded: %q code=%d stdout=%q stderr=%q", query, code, stdout.String(), stderr.String())
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow(`SELECT title FROM events`).Scan(&title); err != nil || title != "Original" {
		t.Fatalf("SQL command changed event: title=%q err=%v", title, err)
	}
	var injected, version int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'injected'`).Scan(&injected); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if injected != 0 || version != 4 {
		t.Fatalf("SQL command changed schema: injected=%d user_version=%d", injected, version)
	}
}

func TestSQLAcceptsSafePragmaAndRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	pragma := runOK(t, root, "sql", `PRAGMA table_info(events)`, "--json")
	var result struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	if err := json.Unmarshal([]byte(pragma), &result); err != nil || len(result.Rows) == 0 || len(result.Columns) == 0 {
		t.Fatalf("safe row-returning PRAGMA failed: err=%v result=%#v", err, result)
	}

	invalid := []string{"", "   ", strings.Repeat("x", 64*1024+1), `PRAGMA journal_mode(WAL)`, `INSERT INTO events DEFAULT VALUES`}
	for _, query := range invalid {
		var stdout, stderr bytes.Buffer
		if code := app.Run([]string{"sql", query}, root, &stdout, &stderr); code == 0 || stderr.Len() == 0 {
			t.Fatalf("invalid query accepted: %q code=%d stderr=%q", query, code, stderr.String())
		}
	}
}

func TestAuthoritativeEventsRejectMutation(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	runOK(t, root, "add", "audit", "--title", "Original", "--body", "Must remain")

	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE events SET title = 'Changed'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE error = %v, want append-only rejection", err)
	}
	if _, err := db.Exec(`DELETE FROM events`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE error = %v, want append-only rejection", err)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM events`).Scan(&title); err != nil || title != "Original" {
		t.Fatalf("event changed: title=%q err=%v", title, err)
	}
}

func TestInvalidRevisionArgumentsDoNotAppend(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	entryID := strings.TrimSpace(runOK(t, root, "add", "notes", "--title", "One", "--body", "Body"))
	cases := [][]string{
		{"replace", "notes", entryID},
		{"replace", "notes", entryID, "--title", "   "},
		{"replace", "notes", entryID, "--body", "next", "--based-on", "not-an-id"},
		{"delete", "notes", entryID, "--reason", "   "},
		{"delete", "bad name", entryID, "--reason", "invalid ledger"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := app.Run(args, root, &stdout, &stderr); code == 0 || stderr.Len() == 0 {
			t.Fatalf("ledger %v: code=%d stderr=%q, want useful validation error", args, code, stderr.String())
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid revisions appended events: count=%d err=%v", count, err)
	}
}

func TestInvalidAddDoesNotWriteAndReportsUsageError(t *testing.T) {
	root := t.TempDir()
	runOK(t, root, "init")
	var stdout, stderr bytes.Buffer
	code := app.Run([]string{"add", "bad name", "--title", "", "--body", "body"}, root, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "ledger name") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}

	db, err := sql.Open("sqlite", filepath.Join(root, ".ledger", "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid add persisted events: count=%d err=%v", count, err)
	}
}

func TestInitCreatesProjectDatabaseAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	for attempt := 0; attempt < 2; attempt++ {
		var stdout, stderr bytes.Buffer
		if code := app.Run([]string{"init"}, dir, &stdout, &stderr); code != 0 {
			t.Fatalf("attempt %d: exit code = %d, stderr = %q", attempt+1, code, stderr.String())
		}
	}

	dbPath := filepath.Join(dir, ".ledger", "ledger.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created: %v", err)
	}
}

func runOK(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := app.Run(args, cwd, &stdout, &stderr); code != 0 {
		t.Fatalf("ledger %v: exit code = %d, stderr = %q", args, code, stderr.String())
	}
	return stdout.String()
}
