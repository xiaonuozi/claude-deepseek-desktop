package logstore

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenLogStoreMigratesLegacySchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE runs (
	id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	project_path TEXT NOT NULL,
	model TEXT NOT NULL,
	permission_mode TEXT NOT NULL,
	prompt TEXT NOT NULL,
	display_output TEXT NOT NULL,
	raw_output TEXT NOT NULL,
	exit_code INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL
);
INSERT INTO runs (
	id, created_at, project_path, model, permission_mode, prompt, display_output, raw_output, exit_code, duration_ms
) VALUES (
	'run-1', '2026-04-28T09:00:00+08:00', 'C:\test', 'deepseek-v4-pro', 'default', 'hello', 'world', '{}', 0, 42
);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()

	entries, err := store.GetRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ThreadID != "run-1" {
		t.Fatalf("expected migrated thread id run-1, got %q", entries[0].ThreadID)
	}
	if entries[0].DisplayOutput != "" || entries[0].RawOutput != "" {
		t.Fatalf("recent logs should not include heavy payloads, got display=%q raw=%q", entries[0].DisplayOutput, entries[0].RawOutput)
	}
}

func TestGetThreadLimitsHeavyPayloads(t *testing.T) {
	dir := t.TempDir()
	store, err := openLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()

	if err := store.Append(LogEntry{
		ID:             "run-heavy",
		ThreadID:       "thread-heavy",
		CreatedAt:      "2026-04-28T09:00:00+08:00",
		ProjectPath:    `C:\test`,
		Model:          "deepseek-v4-pro",
		PermissionMode: "default",
		Prompt:         "heavy",
		DisplayOutput:  strings.Repeat("d", maxFrontendDisplayChars+1000),
		RawOutput:      strings.Repeat("r", maxFrontendRawChars+1000),
		ExitCode:       0,
		DurationMS:     42,
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := store.GetThread("thread-heavy")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if len(entries[0].DisplayOutput) > maxFrontendDisplayChars {
		t.Fatalf("display payload was not limited: %d", len(entries[0].DisplayOutput))
	}
	if len(entries[0].RawOutput) > maxFrontendRawChars {
		t.Fatalf("raw payload was not limited: %d", len(entries[0].RawOutput))
	}
}

func TestDeleteThreadsPrunesLegacyJSONL(t *testing.T) {
	dir := t.TempDir()
	deleted := LogEntry{
		ID:             "run-delete",
		ThreadID:       "thread-delete",
		CreatedAt:      "2026-04-28T09:00:00+08:00",
		ProjectPath:    `C:\test`,
		Model:          "deepseek-v4-pro",
		PermissionMode: "default",
		Prompt:         "delete me",
		DisplayOutput:  "bye",
		RawOutput:      "{}",
		ExitCode:       0,
		DurationMS:     42,
	}
	kept := deleted
	kept.ID = "run-keep"
	kept.ThreadID = "thread-keep"
	kept.Prompt = "keep me"

	legacyLines := []string{}
	for _, entry := range []LogEntry{deleted, kept} {
		b, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		legacyLines = append(legacyLines, string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "runs.jsonl"), []byte(strings.Join(legacyLines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	store, err := openLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	deletedRows, err := store.DeleteThreads("thread-delete")
	if err != nil {
		t.Fatal(err)
	}
	if deletedRows != 1 {
		t.Fatalf("expected 1 deleted row, got %d", deletedRows)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()

	entries, err := reopened.GetRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after reopen, got %d", len(entries))
	}
	if entries[0].ThreadID != "thread-keep" {
		t.Fatalf("expected kept thread after reopen, got %q", entries[0].ThreadID)
	}

	legacyContent, err := os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacyContent), "thread-delete") {
		t.Fatalf("expected legacy JSONL to remove deleted thread, got %s", legacyContent)
	}
}

func TestOpenLogStoreBackfillsTokenUsage(t *testing.T) {
	dir := t.TempDir()
	raw := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg-1","usage":{"input_tokens":10,"output_tokens":0}}}}`,
		`{"type":"assistant","message":{"id":"msg-1","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"assistant","message":{"id":"msg-2","usage":{"input_tokens":20,"output_tokens":7},"content":[{"type":"text","text":"world"}]}}`,
	}, "\n")

	db, err := sql.Open("sqlite", filepath.Join(dir, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE runs (
	id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	project_path TEXT NOT NULL,
	model TEXT NOT NULL,
	permission_mode TEXT NOT NULL,
	prompt TEXT NOT NULL,
	display_output TEXT NOT NULL,
	raw_output TEXT NOT NULL,
	exit_code INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL
);`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
INSERT INTO runs (
	id, created_at, project_path, model, permission_mode, prompt, display_output, raw_output, exit_code, duration_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"run-token",
		"2026-04-28T09:00:00+08:00",
		`C:\test`,
		"deepseek-v4-pro",
		"default",
		"hello",
		"world",
		raw,
		0,
		42,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()

	entries, err := store.GetRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].InputTokens != 30 || entries[0].OutputTokens != 12 {
		t.Fatalf("expected token usage 30/12, got %d/%d", entries[0].InputTokens, entries[0].OutputTokens)
	}
}
