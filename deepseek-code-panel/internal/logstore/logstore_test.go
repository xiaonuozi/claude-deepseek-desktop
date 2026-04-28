package logstore

import (
	"database/sql"
	"path/filepath"
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
}
