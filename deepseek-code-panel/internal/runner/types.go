package runner

// RunRequest is the request from frontend to start a claude run.
type RunRequest struct {
	RunID           string `json:"run_id"`
	ProjectPath     string `json:"project_path"`
	ThreadID        string `json:"thread_id"`
	ClaudeSessionID string `json:"claude_session_id"`
	Prompt          string `json:"prompt"`
	APIKey          string `json:"api_key"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	PermissionMode  string `json:"permission_mode"`
	Language        string `json:"language"`
}

// RunEvent is emitted to the frontend in real time via Wails EventsEmit.
type RunEvent struct {
	Type            string                 `json:"type"`
	Text            string                 `json:"text"`
	Raw             string                 `json:"raw,omitempty"`
	RunID           string                 `json:"run_id"`
	ThreadID        string                 `json:"thread_id"`
	ClaudeSessionID string                 `json:"claude_session_id,omitempty"`
	Timestamp       string                 `json:"timestamp"`
	Meta            map[string]interface{} `json:"meta,omitempty"`
}

// RunLog is persisted to SQLite after each completed run.
type RunLog struct {
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
