package logstore

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxFrontendDisplayChars  = 500000
	maxFrontendRawChars      = 700000
	maxTokenBackfillRawChars = 8_000_000
	truncatedPayloadNote     = "\n\n[内容过长，界面仅显示截断预览；完整 raw 流请查看项目 .claude-tools/runs]\n"
)

var (
	instance *LogStore
	initMu   sync.Mutex
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	thread_id TEXT NOT NULL DEFAULT '',
	claude_session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	project_path TEXT NOT NULL,
	model TEXT NOT NULL,
	permission_mode TEXT NOT NULL,
	prompt TEXT NOT NULL,
	display_output TEXT NOT NULL,
	raw_output TEXT NOT NULL,
	exit_code INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_project_path ON runs(project_path);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);
`

// Init initializes the global log store singleton. Safe to call multiple times.
func Init() error {
	initMu.Lock()
	defer initMu.Unlock()

	if instance != nil && instance.db != nil {
		if err := instance.db.Ping(); err == nil {
			return nil
		}
		_ = instance.db.Close()
		instance = nil
	}

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("无法获取用户配置目录: %w", err)
	}
	dir := filepath.Join(cfgDir, "deepseek-code-panel", "logs")
	store, err := openLogStore(dir)
	if err != nil {
		return err
	}
	instance = store
	return nil
}

func globalStore() (*LogStore, error) {
	if err := Init(); err != nil {
		return nil, err
	}

	initMu.Lock()
	defer initMu.Unlock()
	if instance == nil || instance.db == nil {
		return nil, fmt.Errorf("日志库未初始化")
	}
	return instance, nil
}

// AppendLog appends a log entry using the global SQLite store and mirrors it to the project directory.
func AppendLog(entry LogEntry) error {
	store, err := globalStore()
	if err != nil {
		return err
	}
	if err := store.Append(entry); err != nil {
		return err
	}
	return appendProjectArtifacts(entry)
}

// AppendProjectLogLine appends a human-readable run lifecycle line under the selected project directory.
func AppendProjectLogLine(projectPath, runID, message string) error {
	if strings.TrimSpace(projectPath) == "" {
		return nil
	}
	dir := filepath.Join(projectPath, ".claude-tools")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("无法创建项目日志目录 %s: %w", dir, err)
	}
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format(time.RFC3339), runID, message)
	f, err := os.OpenFile(filepath.Join(dir, "runs.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开项目日志失败: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("写入项目日志失败: %w", err)
	}
	return nil
}

// GetRecentLogs returns recent logs from the global store.
func GetRecentLogs(limit int) ([]LogEntry, error) {
	store, err := globalStore()
	if err != nil {
		return nil, err
	}
	return store.GetRecent(limit)
}

// LogEntry is a single run log record.
type LogEntry struct {
	ID              string `json:"id"`
	ThreadID        string `json:"thread_id"`
	ClaudeSessionID string `json:"claude_session_id"`
	CreatedAt       string `json:"created_at"`
	ProjectPath     string `json:"project_path"`
	Model           string `json:"model"`
	PermissionMode  string `json:"permission_mode"`
	Prompt          string `json:"prompt"`
	DisplayOutput   string `json:"display_output"`
	RawOutput       string `json:"raw_output"`
	ExitCode        int    `json:"exit_code"`
	DurationMS      int64  `json:"duration_ms"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
}

// LogStore provides read/write access to a SQLite run log.
type LogStore struct {
	dir  string
	path string
	db   *sql.DB
}

// NewLogStore creates a global LogStore in the user config directory.
func NewLogStore() (*LogStore, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("无法获取用户配置目录: %w", err)
	}
	return openLogStore(filepath.Join(cfgDir, "deepseek-code-panel", "logs"))
}

func openLogStore(dir string) (*LogStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("无法创建日志目录 %s: %w", dir, err)
	}
	path := filepath.Join(dir, "runs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 日志失败: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 SQLite 日志失败: %w", err)
	}
	if err := migrateColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateJSONL(db, filepath.Join(dir, "runs.jsonl")); err != nil {
		db.Close()
		return nil, err
	}
	return &LogStore{dir: dir, path: path, db: db}, nil
}

// Append adds a log entry to the SQLite database.
func (ls *LogStore) Append(entry LogEntry) error {
	if ls == nil || ls.db == nil {
		return fmt.Errorf("日志库未初始化")
	}
	return insertEntry(ls.db, entry)
}

// GetRecent returns the most recent N log entries (newest first).
func (ls *LogStore) GetRecent(limit int) ([]LogEntry, error) {
	if ls == nil || ls.db == nil {
		return nil, fmt.Errorf("日志库未初始化")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := ls.db.Query(`
SELECT r.id, r.thread_id, r.claude_session_id, r.created_at, r.project_path, r.model, r.permission_mode, r.prompt, '', '', r.exit_code, r.duration_ms, r.input_tokens, r.output_tokens
FROM runs r
JOIN (
	SELECT COALESCE(NULLIF(thread_id, ''), id) AS thread_key, MAX(created_at) AS latest_created_at
	FROM runs
	GROUP BY thread_key
) latest
ON COALESCE(NULLIF(r.thread_id, ''), r.id) = latest.thread_key AND r.created_at = latest.latest_created_at
ORDER BY r.created_at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("读取 SQLite 日志失败: %w", err)
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var entry LogEntry
		if err := rows.Scan(
			&entry.ID,
			&entry.ThreadID,
			&entry.ClaudeSessionID,
			&entry.CreatedAt,
			&entry.ProjectPath,
			&entry.Model,
			&entry.PermissionMode,
			&entry.Prompt,
			&entry.DisplayOutput,
			&entry.RawOutput,
			&entry.ExitCode,
			&entry.DurationMS,
			&entry.InputTokens,
			&entry.OutputTokens,
		); err != nil {
			return nil, fmt.Errorf("解析 SQLite 日志失败: %w", err)
		}
		if entry.ThreadID == "" {
			entry.ThreadID = entry.ID
		}
		limitLogEntryPayload(&entry)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 SQLite 日志失败: %w", err)
	}
	return entries, nil
}

// GetThread returns all runs in a thread from oldest to newest.
func (ls *LogStore) GetThread(threadID string) ([]LogEntry, error) {
	if ls == nil || ls.db == nil {
		return nil, fmt.Errorf("日志库未初始化")
	}
	displayKeep := maxFrontendDisplayChars - len(truncatedPayloadNote)
	if displayKeep < 0 {
		displayKeep = 0
	}
	rawKeep := maxFrontendRawChars - len(truncatedPayloadNote)
	if rawKeep < 0 {
		rawKeep = 0
	}
	rows, err := ls.db.Query(`
SELECT id, thread_id, claude_session_id, created_at, project_path, model, permission_mode, prompt,
	CASE
		WHEN length(display_output) > ? THEN substr(display_output, 1, ?) || ?
		ELSE display_output
	END,
	CASE
		WHEN length(raw_output) > ? THEN ? || substr(raw_output, length(raw_output) - ? + 1)
		ELSE raw_output
	END,
	exit_code, duration_ms, input_tokens, output_tokens
FROM runs
WHERE COALESCE(NULLIF(thread_id, ''), id) = ?
ORDER BY created_at ASC`,
		maxFrontendDisplayChars,
		displayKeep,
		truncatedPayloadNote,
		maxFrontendRawChars,
		truncatedPayloadNote,
		rawKeep,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("读取线程日志失败: %w", err)
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var entry LogEntry
		if err := rows.Scan(
			&entry.ID,
			&entry.ThreadID,
			&entry.ClaudeSessionID,
			&entry.CreatedAt,
			&entry.ProjectPath,
			&entry.Model,
			&entry.PermissionMode,
			&entry.Prompt,
			&entry.DisplayOutput,
			&entry.RawOutput,
			&entry.ExitCode,
			&entry.DurationMS,
			&entry.InputTokens,
			&entry.OutputTokens,
		); err != nil {
			return nil, fmt.Errorf("解析线程日志失败: %w", err)
		}
		if entry.ThreadID == "" {
			entry.ThreadID = entry.ID
		}
		limitLogEntryPayload(&entry)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取线程日志失败: %w", err)
	}
	return entries, nil
}

// GetLatestSessionID returns the newest Claude session id for a thread.
func (ls *LogStore) GetLatestSessionID(threadID string) (string, error) {
	if ls == nil || ls.db == nil {
		return "", fmt.Errorf("日志库未初始化")
	}
	var sessionID string
	err := ls.db.QueryRow(`
SELECT claude_session_id
FROM runs
WHERE COALESCE(NULLIF(thread_id, ''), id) = ? AND claude_session_id <> ''
ORDER BY created_at DESC
LIMIT 1`, threadID).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取 Claude session 失败: %w", err)
	}
	return sessionID, nil
}

// GetLogDir returns the log directory path.
func (ls *LogStore) GetLogDir() string {
	return ls.dir
}

// DeleteThreads removes all runs belonging to the given thread.
func (ls *LogStore) DeleteThreads(threadID string) (int64, error) {
	if ls == nil || ls.db == nil {
		return 0, fmt.Errorf("日志库未初始化")
	}
	result, err := ls.db.Exec(`DELETE FROM runs WHERE COALESCE(NULLIF(thread_id, ''), id) = ?`, threadID)
	if err != nil {
		return 0, fmt.Errorf("删除线程日志失败: %w", err)
	}
	n, _ := result.RowsAffected()
	if err := pruneJSONLThread(filepath.Join(ls.dir, "runs.jsonl"), threadID); err != nil {
		return n, err
	}
	return n, nil
}

func (ls *LogStore) SetSetting(key, value string) error {
	if ls == nil || ls.db == nil {
		return fmt.Errorf("日志库未初始化")
	}
	_, err := ls.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (ls *LogStore) GetSetting(key string) (string, error) {
	if ls == nil || ls.db == nil {
		return "", fmt.Errorf("日志库未初始化")
	}
	var value string
	err := ls.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func insertEntry(db *sql.DB, entry LogEntry) error {
	if strings.TrimSpace(entry.ThreadID) == "" {
		entry.ThreadID = entry.ID
	}
	_, err := db.Exec(`
INSERT INTO runs (
	id, thread_id, claude_session_id, created_at, project_path, model, permission_mode, prompt, display_output, raw_output, exit_code, duration_ms, input_tokens, output_tokens
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	thread_id = excluded.thread_id,
	claude_session_id = excluded.claude_session_id,
	created_at = excluded.created_at,
	project_path = excluded.project_path,
	model = excluded.model,
	permission_mode = excluded.permission_mode,
	prompt = excluded.prompt,
	display_output = excluded.display_output,
	raw_output = excluded.raw_output,
	exit_code = excluded.exit_code,
	duration_ms = excluded.duration_ms,
	input_tokens = excluded.input_tokens,
	output_tokens = excluded.output_tokens`,
		entry.ID,
		entry.ThreadID,
		entry.ClaudeSessionID,
		entry.CreatedAt,
		entry.ProjectPath,
		entry.Model,
		entry.PermissionMode,
		entry.Prompt,
		entry.DisplayOutput,
		entry.RawOutput,
		entry.ExitCode,
		entry.DurationMS,
		entry.InputTokens,
		entry.OutputTokens,
	)
	if err != nil {
		return fmt.Errorf("写入 SQLite 日志失败: %w", err)
	}
	return nil
}

// GetThreadLogs returns all logs in one thread from the global store.
func GetThreadLogs(threadID string) ([]LogEntry, error) {
	store, err := globalStore()
	if err != nil {
		return nil, err
	}
	return store.GetThread(threadID)
}

// GetLatestSessionID returns the newest Claude session id in one thread from the global store.
func GetLatestSessionID(threadID string) (string, error) {
	store, err := globalStore()
	if err != nil {
		return "", err
	}
	return store.GetLatestSessionID(threadID)
}

// DeleteThreads removes all runs belonging to the given thread from the global store.
func DeleteThreads(threadID string) (int64, error) {
	store, err := globalStore()
	if err != nil {
		return 0, err
	}
	return store.DeleteThreads(threadID)
}

// SetSetting persists a key-value setting.
func SetSetting(key, value string) error {
	store, err := globalStore()
	if err != nil {
		return err
	}
	return store.SetSetting(key, value)
}

// GetSetting retrieves a setting value.
func GetSetting(key string) (string, error) {
	store, err := globalStore()
	if err != nil {
		return "", err
	}
	return store.GetSetting(key)
}

func appendProjectArtifacts(entry LogEntry) error {
	if strings.TrimSpace(entry.ProjectPath) == "" {
		return nil
	}
	projectDir := filepath.Join(entry.ProjectPath, ".claude-tools")
	projectStore, err := openLogStore(projectDir)
	if err != nil {
		return err
	}
	defer projectStore.db.Close()

	if err := projectStore.Append(entry); err != nil {
		return err
	}

	summary := fmt.Sprintf(
		"finish: thread=%s session=%s exit_code=%d duration=%dms model=%s prompt=%q",
		entry.ThreadID,
		entry.ClaudeSessionID,
		entry.ExitCode,
		entry.DurationMS,
		entry.Model,
		truncate(entry.Prompt, 120),
	)
	return AppendProjectLogLine(entry.ProjectPath, entry.ID, summary)
}

func migrateColumns(db *sql.DB) error {
	columns, err := tableColumns(db, "runs")
	if err != nil {
		return err
	}
	if !columns["thread_id"] {
		if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN thread_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("迁移 SQLite 日志 thread_id 失败: %w", err)
		}
	}
	if !columns["claude_session_id"] {
		if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN claude_session_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("迁移 SQLite 日志 claude_session_id 失败: %w", err)
		}
	}
	if !columns["input_tokens"] {
		if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("迁移 SQLite 日志 input_tokens 失败: %w", err)
		}
	}
	if !columns["output_tokens"] {
		if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("迁移 SQLite 日志 output_tokens 失败: %w", err)
		}
	}
	if _, err := db.Exec(`UPDATE runs SET thread_id = id WHERE thread_id = ''`); err != nil {
		return fmt.Errorf("修复 SQLite 线程 id 失败: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_runs_thread_id ON runs(thread_id)`); err != nil {
		return fmt.Errorf("创建 SQLite 线程索引失败: %w", err)
	}
	if err := backfillTokenUsage(db); err != nil {
		return err
	}
	return nil
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, fmt.Errorf("读取 SQLite 表结构失败: %w", err)
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, fmt.Errorf("解析 SQLite 表结构失败: %w", err)
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func migrateJSONL(db *sql.DB, path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("打开旧 JSONL 日志失败: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var entry LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.ID == "" {
			continue
		}
		if entry.ThreadID == "" {
			entry.ThreadID = entry.ID
		}
		if entry.InputTokens == 0 && entry.OutputTokens == 0 {
			entry.InputTokens, entry.OutputTokens = extractTokenUsageFromRaw(entry.RawOutput)
		}
		_ = insertEntry(db, entry)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取旧 JSONL 日志失败: %w", err)
	}
	return nil
}

func pruneJSONLThread(path, threadID string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("打开旧 JSONL 日志失败: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".runs-*.jsonl")
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("创建临时 JSONL 日志失败: %w", err)
	}
	tmpName := tmp.Name()
	removed := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		keep := true
		var entry LogEntry
		if err := json.Unmarshal(line, &entry); err == nil && entry.ID != "" {
			entryThreadID := entry.ThreadID
			if entryThreadID == "" {
				entryThreadID = entry.ID
			}
			if entryThreadID == threadID {
				keep = false
				removed = true
			}
		}
		if keep {
			if _, err := tmp.Write(line); err != nil {
				_ = f.Close()
				_ = tmp.Close()
				_ = os.Remove(tmpName)
				return fmt.Errorf("写入临时 JSONL 日志失败: %w", err)
			}
			if _, err := tmp.WriteString("\n"); err != nil {
				_ = f.Close()
				_ = tmp.Close()
				_ = os.Remove(tmpName)
				return fmt.Errorf("写入临时 JSONL 日志失败: %w", err)
			}
		}
	}
	scanErr := scanner.Err()
	closeErr := f.Close()
	tmpCloseErr := tmp.Close()
	if scanErr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("读取旧 JSONL 日志失败: %w", scanErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("关闭旧 JSONL 日志失败: %w", closeErr)
	}
	if tmpCloseErr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("关闭临时 JSONL 日志失败: %w", tmpCloseErr)
	}
	if !removed {
		_ = os.Remove(tmpName)
		return nil
	}

	backup := path + ".bak-delete"
	_ = os.Remove(backup)
	if err := os.Rename(path, backup); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("备份旧 JSONL 日志失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Rename(backup, path)
		_ = os.Remove(tmpName)
		return fmt.Errorf("替换旧 JSONL 日志失败: %w", err)
	}
	_ = os.Remove(backup)
	return nil
}

func backfillTokenUsage(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, raw_output FROM runs WHERE input_tokens = 0 AND output_tokens = 0 AND raw_output <> '' AND length(raw_output) <= ?`, maxTokenBackfillRawChars)
	if err != nil {
		return fmt.Errorf("读取 token 回填数据失败: %w", err)
	}
	type tokenUpdate struct {
		id     string
		input  int
		output int
	}
	var updates []tokenUpdate
	for rows.Next() {
		var id string
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return fmt.Errorf("解析 token 回填数据失败: %w", err)
		}
		input, output := extractTokenUsageFromRaw(raw)
		if input > 0 || output > 0 {
			updates = append(updates, tokenUpdate{id: id, input: input, output: output})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("读取 token 回填数据失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭 token 回填数据失败: %w", err)
	}
	for _, update := range updates {
		if _, err := db.Exec(`UPDATE runs SET input_tokens = ?, output_tokens = ? WHERE id = ?`, update.input, update.output, update.id); err != nil {
			return fmt.Errorf("写入 token 回填数据失败: %w", err)
		}
	}
	return nil
}

type tokenUsage struct {
	input  int
	output int
}

func extractTokenUsageFromRaw(raw string) (int, int) {
	usageByMessage := map[string]tokenUsage{}
	fallback := tokenUsage{}

	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		recordTokenUsage(obj, usageByMessage, &fallback)
		if event, ok := obj["event"].(map[string]interface{}); ok {
			recordTokenUsage(event, usageByMessage, &fallback)
		}
	}

	if len(usageByMessage) == 0 {
		return fallback.input, fallback.output
	}
	total := tokenUsage{}
	for _, usage := range usageByMessage {
		total.input += usage.input
		total.output += usage.output
	}
	return total.input, total.output
}

func recordTokenUsage(obj map[string]interface{}, usageByMessage map[string]tokenUsage, fallback *tokenUsage) {
	if msg, ok := obj["message"].(map[string]interface{}); ok {
		if usage, ok := msg["usage"].(map[string]interface{}); ok {
			id, _ := msg["id"].(string)
			tokenUsage := tokenUsageFromMap(usage)
			if id != "" {
				usageByMessage[id] = tokenUsage
				return
			}
			if tokenUsage.input > fallback.input {
				fallback.input = tokenUsage.input
			}
			if tokenUsage.output > fallback.output {
				fallback.output = tokenUsage.output
			}
		}
	}
	if usage, ok := obj["usage"].(map[string]interface{}); ok {
		tokenUsage := tokenUsageFromMap(usage)
		if tokenUsage.input > fallback.input {
			fallback.input = tokenUsage.input
		}
		if tokenUsage.output > fallback.output {
			fallback.output = tokenUsage.output
		}
	}
}

func tokenUsageFromMap(usage map[string]interface{}) tokenUsage {
	return tokenUsage{
		input:  intFromJSONNumber(usage["input_tokens"]),
		output: intFromJSONNumber(usage["output_tokens"]),
	}
}

func intFromJSONNumber(value interface{}) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}

func truncate(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "..."
}

func limitLogEntryPayload(entry *LogEntry) {
	entry.DisplayOutput = truncatePayloadHead(entry.DisplayOutput, maxFrontendDisplayChars)
	entry.RawOutput = truncatePayloadTail(entry.RawOutput, maxFrontendRawChars)
}

func truncatePayloadHead(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	note := truncatedPayloadNote
	keep := limit - len(note)
	if keep <= 0 {
		return value[:limit]
	}
	return value[:keep] + note
}

func truncatePayloadTail(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	note := truncatedPayloadNote
	keep := limit - len(note)
	if keep <= 0 {
		return value[len(value)-limit:]
	}
	return note + value[len(value)-keep:]
}
