package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"deepseek-code-panel/internal/logstore"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	maxFrontendEventRawChars  = 12000
	maxFrontendEventTextChars = 80000
	frontendTruncatedNote     = "\n[内容过长，前端事件已截断]\n"
	maxDisplayChars           = 2_000_000
	maxRawChars               = 5_000_000
	maxParserJSONLineChars    = 1_000_000
	maxParserStateChars       = 500_000
	maxParserMetaChars        = 20_000
	diagnosticByteInterval    = 1_000_000
	truncatedLogNote          = "\n\n[内容过长，内存预览已截断；如需完整 raw 流，可设置 CLAUDE_TOOLS_RAW_LOG=1 后重试]\n"
	parserTruncatedNote       = "\n[内容过长，parser 状态已截断]\n"
)

// Runner manages concurrent claude CLI processes.
type Runner struct {
	mu   sync.Mutex
	runs map[string]context.CancelFunc // runID → cancel
}

type cappedTextBuffer struct {
	builder   strings.Builder
	limit     int
	note      string
	seenBytes int
	truncated bool
}

func newCappedTextBuffer(limit int, note string) *cappedTextBuffer {
	return &cappedTextBuffer{limit: limit, note: note}
}

func (b *cappedTextBuffer) Append(value string) bool {
	if value == "" {
		return false
	}
	b.seenBytes += len(value)
	if b.limit <= 0 || b.truncated {
		return false
	}
	if b.builder.Len()+len(value) <= b.limit {
		b.builder.WriteString(value)
		return false
	}
	keep := b.limit - b.builder.Len() - len(b.note)
	if keep > 0 {
		if keep > len(value) {
			keep = len(value)
		}
		b.builder.WriteString(value[:keep])
	}
	if b.builder.Len()+len(b.note) <= b.limit {
		b.builder.WriteString(b.note)
	}
	b.truncated = true
	return true
}

func (b *cappedTextBuffer) String() string {
	return b.builder.String()
}

func (b *cappedTextBuffer) SeenBytes() int {
	return b.seenBytes
}

func (b *cappedTextBuffer) KeptBytes() int {
	return b.builder.Len()
}

func (b *cappedTextBuffer) Truncated() bool {
	return b.truncated
}

type runCounters struct {
	stdoutLines            int
	stderrLines            int
	stdoutBytes            int
	stderrBytes            int
	maxStdoutLineBytes     int
	maxStderrLineBytes     int
	displayBytes           int
	frontendTextTruncated  int
	frontendRawTruncated   int
	suppressedStdoutEvents int
	suppressedStdoutBytes  int
	eventsEmitted          int
	parserWarnings         int
	nextDiagnosticByteMark int
}

// NewRunner creates a new Runner.
func NewRunner() *Runner {
	return &Runner{runs: map[string]context.CancelFunc{}}
}

// IsRunning returns whether any run is currently in progress.
func (r *Runner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs) > 0
}

// Start launches the claude CLI with the given request and streams output via Wails events.
// Multiple runs can execute concurrently.
func (r *Runner) Start(ctx context.Context, req RunRequest, runID string) {
	go r.run(ctx, req, runID)
}

// Stop terminates a specific run, or all runs if runID is empty.
func (r *Runner) Stop(ctx context.Context, runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if runID != "" {
		if cancel, ok := r.runs[runID]; ok {
			cancel()
			delete(r.runs, runID)
			return nil
		}
		return fmt.Errorf("未找到运行中的任务: %s", runID)
	}
	// Stop all
	if len(r.runs) == 0 {
		return fmt.Errorf("没有正在运行的任务")
	}
	for id, cancel := range r.runs {
		cancel()
		delete(r.runs, id)
	}
	return nil
}

func appendRunDiagnostic(projectPath, runID, message string) {
	if !envFlag("CLAUDE_TOOLS_DIAGNOSTICS") {
		return
	}
	var ms goruntime.MemStats
	goruntime.ReadMemStats(&ms)
	line := fmt.Sprintf(
		"diagnostic: %s mem_alloc=%s heap_inuse=%s heap_sys=%s gc=%d goroutines=%d",
		message,
		formatBytes(ms.Alloc),
		formatBytes(ms.HeapInuse),
		formatBytes(ms.HeapSys),
		ms.NumGC,
		goruntime.NumGoroutine(),
	)
	_ = logstore.AppendProjectLogLine(projectPath, runID, line)
}

func rawLoggingEnabled() bool {
	return envFlag("CLAUDE_TOOLS_RAW_LOG") || envFlag("CLAUDE_TOOLS_DIAGNOSTICS")
}

func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func createRunStreamLog(projectPath, runID string) (*os.File, string, error) {
	if strings.TrimSpace(projectPath) == "" {
		return nil, "", nil
	}
	dir := filepath.Join(projectPath, ".claude-tools", "runs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, "", fmt.Errorf("无法创建 raw 流日志目录 %s: %w", dir, err)
	}
	path := filepath.Join(dir, safeLogFilename(runID)+".stream.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return nil, "", fmt.Errorf("无法创建 raw 流日志 %s: %w", path, err)
	}
	return f, path, nil
}

func writeStreamLog(f *os.File, prefix, line string) {
	if f == nil {
		return
	}
	if prefix != "" {
		_, _ = f.WriteString(prefix)
	}
	_, _ = f.WriteString(line)
	_, _ = f.WriteString("\n")
}

func rawStreamText(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return "完整 raw 流日志: " + path + "\n"
}

func safeLogFilename(value string) string {
	if value == "" {
		return "run"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

func formatBytes(value uint64) string {
	const kb = 1024
	const mb = kb * 1024
	if value >= mb {
		return fmt.Sprintf("%.1fMiB", float64(value)/mb)
	}
	if value >= kb {
		return fmt.Sprintf("%.1fKiB", float64(value)/kb)
	}
	return fmt.Sprintf("%dB", value)
}

func (r *Runner) run(wailsCtx context.Context, req RunRequest, runID string) {
	defer func() {
		if rec := recover(); rec != nil {
			stack := string(debug.Stack())
			_ = logstore.AppendProjectLogLine(req.ProjectPath, runID, fmt.Sprintf("panic: %v\n%s", rec, stack))
			runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
				Type:     "error",
				Text:     fmt.Sprintf("内部错误: %v（详细堆栈已写入项目日志）", rec),
				RunID:    runID,
				ThreadID: req.ThreadID,
			})
		}
		r.mu.Lock()
		delete(r.runs, runID)
		r.mu.Unlock()
	}()

	startTime := time.Now()

	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Language = strings.TrimSpace(req.Language)
	req.PermissionMode = strings.TrimSpace(req.PermissionMode)
	req.Model = strings.TrimSpace(req.Model)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.ThreadID = strings.TrimSpace(req.ThreadID)
	req.ClaudeSessionID = strings.TrimSpace(req.ClaudeSessionID)
	if req.ThreadID == "" {
		req.ThreadID = runID
	}
	if req.ClaudeSessionID == "" {
		if sessionID, err := logstore.GetLatestSessionID(req.ThreadID); err == nil {
			req.ClaudeSessionID = sessionID
		}
	}

	// Build the full prompt with language instruction.
	fullPrompt := req.Prompt
	if strings.TrimSpace(req.Language) != "" {
		fullPrompt = "请使用" + req.Language + "回复，除非我明确要求使用其他语言。\n\n" + fullPrompt
	}

	// Create cancellable context for the claude subprocess
	cmdCtx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.runs[runID] = cancel
	r.mu.Unlock()
	defer cancel()

	args := []string{
		"-p",
		"--input-format", "text",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-mode", req.PermissionMode,
		"--model", req.Model,
	}
	if req.ClaudeSessionID != "" {
		args = append(args, "--resume", req.ClaudeSessionID)
	}
	cmd := exec.CommandContext(cmdCtx, "claude", args...)
	cmd.Stdin = strings.NewReader(fullPrompt + "\n")

	// Set environment variables for DeepSeek API (inherit system env + append custom)
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL="+req.BaseURL,
		"ANTHROPIC_API_KEY="+req.APIKey,
		"ANTHROPIC_AUTH_TOKEN="+req.APIKey,
		"ANTHROPIC_MODEL="+req.Model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL="+req.Model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL="+req.Model,
	)

	// Set working directory to project path
	cmd.Dir = req.ProjectPath
	configureCommand(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = logstore.AppendProjectLogLine(req.ProjectPath, runID, "stdout pipe failed: "+err.Error())
		runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
			Type:     "error",
			Text:     "创建 stdout 管道失败: " + err.Error(),
			RunID:    runID,
			ThreadID: req.ThreadID,
		})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = logstore.AppendProjectLogLine(req.ProjectPath, runID, "stderr pipe failed: "+err.Error())
		runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
			Type:     "error",
			Text:     "创建 stderr 管道失败: " + err.Error(),
			RunID:    runID,
			ThreadID: req.ThreadID,
		})
		return
	}

	if err := logstore.AppendProjectLogLine(req.ProjectPath, runID, fmt.Sprintf(
		"start: thread=%s session=%s model=%s permission=%s prompt_bytes=%d full_prompt_bytes=%d",
		req.ThreadID,
		req.ClaudeSessionID,
		req.Model,
		req.PermissionMode,
		len(req.Prompt),
		len(fullPrompt),
	)); err != nil {
		runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
			Type:     "stderr",
			Text:     "写入项目启动日志失败: " + err.Error(),
			RunID:    runID,
			ThreadID: req.ThreadID,
		})
	}

	var streamLog *os.File
	streamLogPath := ""
	if rawLoggingEnabled() {
		var err error
		streamLog, streamLogPath, err = createRunStreamLog(req.ProjectPath, runID)
		if err != nil {
			_ = logstore.AppendProjectLogLine(req.ProjectPath, runID, "raw stream log failed: "+err.Error())
			runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
				Type:     "stderr",
				Text:     "创建 raw 流日志失败: " + err.Error(),
				RunID:    runID,
				ThreadID: req.ThreadID,
			})
		} else if streamLogPath != "" {
			_ = logstore.AppendProjectLogLine(req.ProjectPath, runID, "raw stream log: "+streamLogPath)
		}
	}
	if streamLog != nil {
		defer streamLog.Close()
	}
	var streamLogMu sync.Mutex
	writeRawLine := func(prefix, line string) {
		streamLogMu.Lock()
		defer streamLogMu.Unlock()
		writeStreamLog(streamLog, prefix, line)
	}
	appendRunDiagnostic(req.ProjectPath, runID, "phase=before_start")

	if err := cmd.Start(); err != nil {
		_ = logstore.AppendProjectLogLine(req.ProjectPath, runID, "start failed: "+err.Error())
		runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
			Type:     "error",
			Text:     "启动 claude 失败: " + err.Error(),
			RunID:    runID,
			ThreadID: req.ThreadID,
		})
		return
	}
	appendRunDiagnostic(req.ProjectPath, runID, fmt.Sprintf("phase=started pid=%d raw_stream=%q", cmd.Process.Pid, streamLogPath))

	// Keep bounded display in memory. Raw stream logging is opt-in via CLAUDE_TOOLS_RAW_LOG=1.
	displayPreview := newCappedTextBuffer(maxDisplayChars, truncatedLogNote)
	rawPreview := newCappedTextBuffer(0, truncatedLogNote)
	counters := runCounters{nextDiagnosticByteMark: diagnosticByteInterval}
	var outputMu sync.Mutex

	// Emit start event
	runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
		Type:            "status",
		Text:            runStartText(req, startTime),
		Raw:             rawStreamText(streamLogPath),
		RunID:           runID,
		ThreadID:        req.ThreadID,
		ClaudeSessionID: req.ClaudeSessionID,
		Timestamp:       startTime.Format(time.RFC3339),
		Meta: map[string]interface{}{
			"raw_stream": streamLogPath,
		},
	})

	var wg sync.WaitGroup
	wg.Add(2)

	parser := newStreamParser()

	// Read stdout line by line
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			writeRawLine("", line)
			rawTruncatedNow := false
			rawSeenAtTruncate := 0
			progressMessage := ""
			outputMu.Lock()
			counters.stdoutLines++
			counters.stdoutBytes += len(line) + 1
			if len(line) > counters.maxStdoutLineBytes {
				counters.maxStdoutLineBytes = len(line)
			}
			rawTruncatedNow = rawPreview.Append(line + "\n")
			if rawTruncatedNow {
				rawSeenAtTruncate = rawPreview.SeenBytes()
			}
			totalBytes := counters.stdoutBytes + counters.stderrBytes
			if totalBytes >= counters.nextDiagnosticByteMark {
				progressMessage = fmt.Sprintf(
					"phase=stream_progress total_stream=%s stdout_lines=%d stderr_lines=%d max_stdout_line=%s max_stderr_line=%s raw_preview=%s/%s display_preview=%s/%s",
					formatBytes(uint64(totalBytes)),
					counters.stdoutLines,
					counters.stderrLines,
					formatBytes(uint64(counters.maxStdoutLineBytes)),
					formatBytes(uint64(counters.maxStderrLineBytes)),
					formatBytes(uint64(rawPreview.KeptBytes())),
					formatBytes(uint64(rawPreview.SeenBytes())),
					formatBytes(uint64(displayPreview.KeptBytes())),
					formatBytes(uint64(displayPreview.SeenBytes())),
				)
				for totalBytes >= counters.nextDiagnosticByteMark {
					counters.nextDiagnosticByteMark += diagnosticByteInterval
				}
			}
			outputMu.Unlock()
			if rawTruncatedNow {
				appendRunDiagnostic(req.ProjectPath, runID, fmt.Sprintf("phase=raw_preview_truncated limit=%s seen=%s stream_log=%q", formatBytes(uint64(maxRawChars)), formatBytes(uint64(rawSeenAtTruncate)), streamLogPath))
			}
			if progressMessage != "" {
				appendRunDiagnostic(req.ProjectPath, runID, progressMessage)
			}

			// Extract user-visible text or compact status from stream-json.
			eventType, display := parser.extract(line)
			meta := parser.meta()
			for _, warning := range parser.drainWarnings() {
				outputMu.Lock()
				counters.parserWarnings++
				outputMu.Unlock()
				appendRunDiagnostic(req.ProjectPath, runID, "phase=parser_warning "+warning)
			}
			now := time.Now().Format(time.RFC3339Nano)

			if display != "" {
				if eventType == "display" {
					displayTruncatedNow := false
					displaySeenAtTruncate := 0
					outputMu.Lock()
					counters.displayBytes += len(display)
					displayTruncatedNow = displayPreview.Append(display)
					if displayTruncatedNow {
						displaySeenAtTruncate = displayPreview.SeenBytes()
					}
					outputMu.Unlock()
					if displayTruncatedNow {
						appendRunDiagnostic(req.ProjectPath, runID, fmt.Sprintf("phase=display_preview_truncated limit=%s seen=%s", formatBytes(uint64(maxDisplayChars)), formatBytes(uint64(displaySeenAtTruncate))))
					}
				}
				frontendDisplay := truncateFrontendPayload(display, maxFrontendEventTextChars)
				outputMu.Lock()
				if len(frontendDisplay) < len(display) {
					counters.frontendTextTruncated++
				}
				counters.eventsEmitted++
				outputMu.Unlock()
				runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
					Type:            eventType,
					Text:            frontendDisplay,
					RunID:           runID,
					ThreadID:        req.ThreadID,
					ClaudeSessionID: parser.sessionID,
					Timestamp:       now,
					Meta:            meta,
				})
			} else {
				outputMu.Lock()
				counters.suppressedStdoutEvents++
				counters.suppressedStdoutBytes += len(line) + 1
				outputMu.Unlock()
			}
		}
		if err := scanner.Err(); err != nil {
			appendRunDiagnostic(req.ProjectPath, runID, "phase=stdout_scanner_error error="+err.Error())
			runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
				Type:     "stderr",
				Text:     "读取 stdout 失败: " + err.Error() + "\n",
				RunID:    runID,
				ThreadID: req.ThreadID,
			})
		}
	}()

	// Read stderr line by line
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			writeRawLine("[STDERR] ", line)
			rawTruncatedNow := false
			rawSeenAtTruncate := 0
			progressMessage := ""
			outputMu.Lock()
			counters.stderrLines++
			counters.stderrBytes += len(line) + len("[STDERR] ") + 1
			if len(line) > counters.maxStderrLineBytes {
				counters.maxStderrLineBytes = len(line)
			}
			rawTruncatedNow = rawPreview.Append("[STDERR] " + line + "\n")
			if rawTruncatedNow {
				rawSeenAtTruncate = rawPreview.SeenBytes()
			}
			totalBytes := counters.stdoutBytes + counters.stderrBytes
			if totalBytes >= counters.nextDiagnosticByteMark {
				progressMessage = fmt.Sprintf(
					"phase=stream_progress total_stream=%s stdout_lines=%d stderr_lines=%d max_stdout_line=%s max_stderr_line=%s raw_preview=%s/%s display_preview=%s/%s",
					formatBytes(uint64(totalBytes)),
					counters.stdoutLines,
					counters.stderrLines,
					formatBytes(uint64(counters.maxStdoutLineBytes)),
					formatBytes(uint64(counters.maxStderrLineBytes)),
					formatBytes(uint64(rawPreview.KeptBytes())),
					formatBytes(uint64(rawPreview.SeenBytes())),
					formatBytes(uint64(displayPreview.KeptBytes())),
					formatBytes(uint64(displayPreview.SeenBytes())),
				)
				for totalBytes >= counters.nextDiagnosticByteMark {
					counters.nextDiagnosticByteMark += diagnosticByteInterval
				}
			}
			outputMu.Unlock()
			if rawTruncatedNow {
				appendRunDiagnostic(req.ProjectPath, runID, fmt.Sprintf("phase=raw_preview_truncated limit=%s seen=%s stream_log=%q", formatBytes(uint64(maxRawChars)), formatBytes(uint64(rawSeenAtTruncate)), streamLogPath))
			}
			if progressMessage != "" {
				appendRunDiagnostic(req.ProjectPath, runID, progressMessage)
			}
			frontendText := truncateFrontendPayload(line, maxFrontendEventTextChars)
			frontendRaw := truncateFrontendPayload(line, maxFrontendEventRawChars)
			outputMu.Lock()
			if len(frontendText) < len(line) {
				counters.frontendTextTruncated++
			}
			if len(frontendRaw) < len(line) {
				counters.frontendRawTruncated++
			}
			counters.eventsEmitted++
			outputMu.Unlock()
			runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
				Type:            "stderr",
				Text:            frontendText,
				Raw:             frontendRaw,
				RunID:           runID,
				ThreadID:        req.ThreadID,
				ClaudeSessionID: parser.sessionID,
				Timestamp:       time.Now().Format(time.RFC3339Nano),
			})
		}
		if err := scanner.Err(); err != nil {
			appendRunDiagnostic(req.ProjectPath, runID, "phase=stderr_scanner_error error="+err.Error())
			runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
				Type:     "stderr",
				Text:     "读取 stderr 失败: " + err.Error() + "\n",
				RunID:    runID,
				ThreadID: req.ThreadID,
			})
		}
	}()

	wg.Wait()
	appendRunDiagnostic(req.ProjectPath, runID, "phase=pipes_drained")

	err = cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	durationMS := time.Since(startTime).Milliseconds()
	outputMu.Lock()
	displayOutput := displayPreview.String()
	rawOutput := rawStreamText(streamLogPath)
	finalCounters := counters
	rawKeptBytes := rawPreview.KeptBytes()
	rawSeenBytes := rawPreview.SeenBytes()
	rawTruncated := rawPreview.Truncated()
	displayKeptBytes := displayPreview.KeptBytes()
	displaySeenBytes := displayPreview.SeenBytes()
	displayTruncated := displayPreview.Truncated()
	outputMu.Unlock()
	claudeSessionID := parser.sessionID
	if claudeSessionID == "" {
		claudeSessionID = req.ClaudeSessionID
	}

	// Collect token usage from parser.
	inputTokens, outputTokens := parser.usage()
	appendRunDiagnostic(req.ProjectPath, runID, fmt.Sprintf(
		"phase=before_persist exit_code=%d duration_ms=%d stdout_lines=%d stderr_lines=%d stdout_bytes=%s stderr_bytes=%s raw_preview=%s/%s raw_truncated=%t display_preview=%s/%s display_truncated=%t frontend_text_truncated=%d frontend_raw_truncated=%d parser_warnings=%d stream_events=%d suppressed_stdout_events=%d suppressed_stdout_bytes=%s stream_log=%q",
		exitCode,
		durationMS,
		finalCounters.stdoutLines,
		finalCounters.stderrLines,
		formatBytes(uint64(finalCounters.stdoutBytes)),
		formatBytes(uint64(finalCounters.stderrBytes)),
		formatBytes(uint64(rawKeptBytes)),
		formatBytes(uint64(rawSeenBytes)),
		rawTruncated,
		formatBytes(uint64(displayKeptBytes)),
		formatBytes(uint64(displaySeenBytes)),
		displayTruncated,
		finalCounters.frontendTextTruncated,
		finalCounters.frontendRawTruncated,
		finalCounters.parserWarnings,
		finalCounters.eventsEmitted,
		finalCounters.suppressedStdoutEvents,
		formatBytes(uint64(finalCounters.suppressedStdoutBytes)),
		streamLogPath,
	))

	// Save log BEFORE emitting done so loadRecentLogs() sees it immediately.
	log := RunLog{
		ID:              runID,
		ThreadID:        req.ThreadID,
		ClaudeSessionID: claudeSessionID,
		CreatedAt:       startTime.Format(time.RFC3339),
		ProjectPath:     req.ProjectPath,
		Model:           req.Model,
		PermissionMode:  req.PermissionMode,
		Prompt:          req.Prompt,
		DisplayOutput:   displayOutput,
		RawOutput:       rawOutput,
		ExitCode:        exitCode,
		DurationMS:      durationMS,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
	}

	if err := logstore.AppendLog(logstore.LogEntry{
		ID:              log.ID,
		ThreadID:        log.ThreadID,
		ClaudeSessionID: log.ClaudeSessionID,
		CreatedAt:       log.CreatedAt,
		ProjectPath:     log.ProjectPath,
		Model:           log.Model,
		PermissionMode:  log.PermissionMode,
		Prompt:          log.Prompt,
		DisplayOutput:   log.DisplayOutput,
		RawOutput:       log.RawOutput,
		ExitCode:        log.ExitCode,
		DurationMS:      log.DurationMS,
		InputTokens:     log.InputTokens,
		OutputTokens:    log.OutputTokens,
	}); err != nil {
		runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
			Type:            "stderr",
			Text:            "保存日志失败: " + err.Error(),
			RunID:           runID,
			ThreadID:        req.ThreadID,
			ClaudeSessionID: claudeSessionID,
		})
	}
	appendRunDiagnostic(req.ProjectPath, runID, "phase=after_persist")

	// Emit done event
	doneText := fmt.Sprintf("耗时 %s", formatDuration(durationMS))
	if inputTokens > 0 || outputTokens > 0 {
		doneText += fmt.Sprintf(" · 输入 %d token · 输出 %d token", inputTokens, outputTokens)
	}
	runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
		Type:            "done",
		Text:            doneText + "\n",
		RunID:           runID,
		ThreadID:        req.ThreadID,
		ClaudeSessionID: claudeSessionID,
		Timestamp:       time.Now().Format(time.RFC3339),
		Meta: map[string]interface{}{
			"exit_code":      exitCode,
			"duration_ms":    durationMS,
			"input_tokens":   inputTokens,
			"output_tokens":  outputTokens,
			"model":          req.Model,
			"permissionMode": req.PermissionMode,
			"created_at":     log.CreatedAt,
			"raw_stream":     streamLogPath,
		},
	})
	appendRunDiagnostic(req.ProjectPath, runID, "phase=done_event_emitted")
}

type streamParser struct {
	tools           map[int]*toolTrace
	thinking        map[int]bool
	thinkingText    map[int]*cappedTextBuffer
	lastMessageText string
	lastMessageSeen int
	lastMessageCut  bool
	sessionID       string
	lastMeta        map[string]interface{}
	inputTokens     int
	outputTokens    int
	usageByMessage  map[string]tokenUsage
	warnings        []string
}

type toolTrace struct {
	name   string
	input  *cappedTextBuffer
	status string
}

type tokenUsage struct {
	input  int
	output int
}

func newStreamParser() *streamParser {
	return &streamParser{
		tools:          map[int]*toolTrace{},
		thinking:       map[int]bool{},
		thinkingText:   map[int]*cappedTextBuffer{},
		usageByMessage: map[string]tokenUsage{},
	}
}

func (p *streamParser) warn(format string, args ...interface{}) {
	p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
}

func (p *streamParser) drainWarnings() []string {
	if len(p.warnings) == 0 {
		return nil
	}
	warnings := p.warnings
	p.warnings = nil
	return warnings
}

func (p *streamParser) resetLastMessage() {
	p.lastMessageText = ""
	p.lastMessageSeen = 0
	p.lastMessageCut = false
}

func (p *streamParser) appendLastMessage(text string) {
	p.lastMessageSeen += len(text)
	if p.lastMessageCut {
		return
	}
	if len(p.lastMessageText)+len(text) <= maxParserStateChars {
		p.lastMessageText += text
		return
	}
	keep := maxParserStateChars - len(p.lastMessageText)
	if keep > 0 {
		p.lastMessageText += text[:keep]
	}
	p.lastMessageCut = true
	p.warn("assistant message state exceeded %s; keeping prefix only", formatBytes(uint64(maxParserStateChars)))
}

func (p *streamParser) diffAssistantText(text string) (string, bool) {
	if text == "" {
		return "", true
	}
	if !p.lastMessageCut {
		if text == p.lastMessageText {
			return "", true
		}
		if strings.HasPrefix(text, p.lastMessageText) {
			suffix := text[len(p.lastMessageText):]
			p.appendLastMessage(suffix)
			return suffix, suffix == ""
		}
		p.lastMessageText = ""
		p.lastMessageSeen = 0
		p.lastMessageCut = false
		p.appendLastMessage(text)
		return text, false
	}
	if strings.HasPrefix(text, p.lastMessageText) {
		if len(text) <= p.lastMessageSeen {
			p.warn("suppressed duplicate final assistant payload bytes=%s after parser state truncation", formatBytes(uint64(len(text))))
			return "", true
		}
		suffix := text[p.lastMessageSeen:]
		p.appendLastMessage(suffix)
		return suffix, suffix == ""
	}
	p.warn("assistant final payload did not match truncated stream prefix; emitting capped frontend payload bytes=%s", formatBytes(uint64(len(text))))
	p.resetLastMessage()
	p.appendLastMessage(text)
	return text, false
}

func (p *streamParser) usage() (int, int) {
	if len(p.usageByMessage) > 0 {
		inputTokens := 0
		outputTokens := 0
		for _, usage := range p.usageByMessage {
			inputTokens += usage.input
			outputTokens += usage.output
		}
		if p.inputTokens > inputTokens {
			inputTokens = p.inputTokens
		}
		if p.outputTokens > outputTokens {
			outputTokens = p.outputTokens
		}
		return inputTokens, outputTokens
	}
	return p.inputTokens, p.outputTokens
}

func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	s := ms / 1000
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	s = s % 60
	return fmt.Sprintf("%dm%ds", m, s)
}

func truncateFrontendPayload(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	note := frontendTruncatedNote
	keep := limit - len(note)
	if keep <= 0 {
		return value[:limit]
	}
	return value[:keep] + note
}

// meta returns the structured metadata for the most recent extract call.
func (p *streamParser) meta() map[string]interface{} {
	m := p.lastMeta
	p.lastMeta = nil
	return m
}

// extract turns Claude stream-json into an event type and display text.
func (p *streamParser) extract(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	if !strings.HasPrefix(line, "{") {
		return "display", line + "\n"
	}

	if len(line) > maxParserJSONLineChars {
		if sessionID := extractSimpleJSONStringField(line, "session_id"); sessionID != "" {
			p.sessionID = sessionID
		}
		if strings.Contains(line, `"tool_result"`) {
			p.warn("skipped oversized tool_result JSON line bytes=%s", formatBytes(uint64(len(line))))
			p.lastMeta = map[string]interface{}{
				"isToolResult": true,
				"truncated":    true,
				"bytes":        len(line),
			}
			return "tool-result", "[工具结果过长，已跳过解析；如需完整 raw 流，可设置 CLAUDE_TOOLS_RAW_LOG=1 后重试]\n"
		}
		p.warn("skipped oversized JSON line bytes=%s", formatBytes(uint64(len(line))))
		return "", ""
	}

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return "display", line + "\n"
	}

	if sessionID, ok := obj["session_id"].(string); ok && sessionID != "" {
		p.sessionID = sessionID
	}
	if event, ok := obj["event"].(map[string]interface{}); ok {
		obj = event
	}
	if sessionID, ok := obj["session_id"].(string); ok && sessionID != "" {
		p.sessionID = sessionID
	}
	p.recordUsage(obj)

	eventType, _ := obj["type"].(string)
	if eventType == "message_start" {
		p.resetLastMessage()
	}
	idx := blockIndex(obj)

	// Handle deltas
	if delta, ok := obj["delta"].(map[string]interface{}); ok {
		deltaType, _ := delta["type"].(string)

		if text, ok := delta["text"].(string); ok && text != "" {
			p.appendLastMessage(text)
			return "display", text
		}

		if deltaType == "thinking_delta" {
			if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
				if tb, exists := p.thinkingText[idx]; exists {
					if tb.Append(thinking) {
						p.warn("thinking meta exceeded %s for block=%d", formatBytes(uint64(maxParserMetaChars)), idx)
					}
				}
				return "thinking-delta", thinking
			}
		}

		if deltaType == "input_json_delta" {
			if partial, ok := delta["partial_json"].(string); ok && partial != "" {
				if tool := p.tools[idx]; tool != nil {
					if tool.input.Append(partial) {
						p.warn("tool input meta exceeded %s for tool=%s block=%d", formatBytes(uint64(maxParserMetaChars)), tool.name, idx)
					}
					if status := describeToolUse(tool.name, tool.input.String()); status != "" && status != tool.status {
						tool.status = status
						p.lastMeta = map[string]interface{}{"phase": "update", "name": tool.name}
						return "tool-start", status
					}
				}
			}
			return "", ""
		}
		return "", ""
	}

	if eventType == "content_block_start" {
		if cb, ok := obj["content_block"].(map[string]interface{}); ok {
			cbType, _ := cb["type"].(string)
			if cbType == "tool_use" {
				name, _ := cb["name"].(string)
				tool := &toolTrace{name: name, input: newCappedTextBuffer(maxParserMetaChars, parserTruncatedNote)}
				if input, ok := cb["input"].(map[string]interface{}); ok {
					if b, err := json.Marshal(input); err == nil {
						if tool.input.Append(string(b)) {
							p.warn("initial tool input meta exceeded %s for tool=%s block=%d", formatBytes(uint64(maxParserMetaChars)), name, idx)
						}
					}
				}
				p.tools[idx] = tool
				p.lastMeta = map[string]interface{}{"phase": "start", "name": name}
				tool.status = describeToolUse(name, tool.input.String())
				if tool.status == "" {
					tool.status = describeToolStart(name)
				}
				return "tool-start", tool.status
			}
			if cbType == "thinking" || cbType == "redacted_thinking" {
				p.thinking[idx] = true
				p.thinkingText[idx] = newCappedTextBuffer(maxParserMetaChars, parserTruncatedNote)
				p.lastMeta = map[string]interface{}{"phase": "start"}
				return "thinking-start", "深度思考中…\n"
			}
		}
	}

	if eventType == "content_block_stop" {
		if tool := p.tools[idx]; tool != nil {
			delete(p.tools, idx)
			input := tool.input.String()
			p.lastMeta = map[string]interface{}{
				"phase":           "end",
				"name":            tool.name,
				"input":           input,
				"input_bytes":     tool.input.SeenBytes(),
				"input_truncated": tool.input.Truncated(),
			}
			if status := describeToolUse(tool.name, input); status != "" {
				return "tool-end", status
			}
			return "", ""
		}
		if p.thinking[idx] {
			delete(p.thinking, idx)
			thinkingBytes := 0
			thinkingTruncated := false
			if tb, exists := p.thinkingText[idx]; exists {
				thinkingBytes = tb.SeenBytes()
				thinkingTruncated = tb.Truncated()
				delete(p.thinkingText, idx)
			}
			p.lastMeta = map[string]interface{}{
				"phase":             "end",
				"content_bytes":     thinkingBytes,
				"content_truncated": thinkingTruncated,
			}
			return "thinking-end", "深度思考结束\n"
		}
	}

	// assistant message content
	if msg, ok := obj["message"].(map[string]interface{}); ok {
		msgRole, _ := msg["role"].(string)
		if contentArr, ok := msg["content"].([]interface{}); ok {
			// Check for tool_result in user messages
			if msgRole == "user" {
				for _, item := range contentArr {
					if contentItem, ok := item.(map[string]interface{}); ok {
						itemType, _ := contentItem["type"].(string)
						if itemType == "tool_result" {
							resultText := ""
							if c, ok := contentItem["content"].(string); ok {
								resultText = c
							} else if cc, ok := contentItem["content"].([]interface{}); ok {
								var sb strings.Builder
								for _, cci := range cc {
									if ccm, ok := cci.(map[string]interface{}); ok {
										if t, ok := ccm["text"].(string); ok {
											sb.WriteString(t)
										}
									}
								}
								resultText = sb.String()
							}
							p.lastMeta = map[string]interface{}{"isToolResult": true}
							return "tool-result", truncateForLog(resultText, 300)
						}
					}
				}
			}

			// Regular text content from assistant
			var parts []string
			for _, item := range contentArr {
				if contentItem, ok := item.(map[string]interface{}); ok {
					if text, ok := contentItem["text"].(string); ok && text != "" {
						parts = append(parts, text)
					}
				}
			}
			if len(parts) > 0 {
				text := strings.Join(parts, "")
				if suffix, duplicate := p.diffAssistantText(text); !duplicate {
					return "display", suffix
				}
				return "", ""
			}
		}
	}

	if result, ok := obj["result"].(string); ok && result != "" {
		if suffix, duplicate := p.diffAssistantText(result); !duplicate {
			return "display", suffix
		}
		return "", ""
	}

	if eventType == "error" || eventType == "aborted" {
		if errMsg, ok := obj["error"].(string); ok && errMsg != "" {
			return "error", "[Error: " + errMsg + "]\n"
		}
		return "error", "[运行被中断]\n"
	}

	return "", ""
}

func (p *streamParser) recordUsage(obj map[string]interface{}) {
	if msg, ok := obj["message"].(map[string]interface{}); ok {
		if msgUsage, ok := msg["usage"].(map[string]interface{}); ok {
			id, _ := msg["id"].(string)
			usage := tokenUsageFromMap(msgUsage)
			if id != "" {
				p.usageByMessage[id] = usage
				return
			}
			p.recordFallbackUsage(usage)
		}
	}
	if usage, ok := obj["usage"].(map[string]interface{}); ok {
		p.recordFallbackUsage(tokenUsageFromMap(usage))
	}
	if delta, ok := obj["delta"].(map[string]interface{}); ok {
		if usage, ok := delta["usage"].(map[string]interface{}); ok {
			p.recordFallbackUsage(tokenUsageFromMap(usage))
		}
	}
}

func (p *streamParser) recordFallbackUsage(usage tokenUsage) {
	if usage.input > p.inputTokens {
		p.inputTokens = usage.input
	}
	if usage.output > p.outputTokens {
		p.outputTokens = usage.output
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

func runStartText(req RunRequest, startTime time.Time) string {
	return fmt.Sprintf("model: %s / mode: %s / time: %s\n", req.Model, req.PermissionMode, startTime.Format(time.RFC3339))
}

func blockIndex(obj map[string]interface{}) int {
	if v, ok := obj["index"].(float64); ok {
		return int(v)
	}
	if v, ok := obj["content_block_index"].(float64); ok {
		return int(v)
	}
	return -1
}

func describeToolStart(name string) string {
	switch name {
	case "Read", "NotebookRead":
		return "正在读取文件…\n"
	case "Write", "NotebookEdit":
		return "正在写入文件…\n"
	case "Edit", "MultiEdit":
		return "正在修改文件…\n"
	case "Grep":
		return "正在搜索代码…\n"
	case "Glob":
		return "正在查找文件…\n"
	case "Bash", "PowerShell":
		return "正在执行命令…\n"
	case "Task":
		return "正在处理子任务…\n"
	case "AskUserQuestion":
		return "等待用户确认…\n"
	case "ExitPlanMode", "EnterPlanMode":
		return "正在生成计划…\n"
	case "TodoWrite":
		return "正在整理任务列表…\n"
	default:
		return fmt.Sprintf("正在执行 %s…\n", name)
	}
}

func describeToolUse(name, inputJSON string) string {
	input := map[string]interface{}{}
	if strings.TrimSpace(inputJSON) != "" {
		_ = json.Unmarshal([]byte(inputJSON), &input)
	}

	path := firstString(input, "file_path", "path", "notebook_path")
	command := firstString(input, "command", "cmd")
	pattern := firstString(input, "pattern", "query")

	switch name {
	case "Read", "NotebookRead":
		if path != "" {
			return "正在读取 " + path + "\n"
		}
		return "正在读取文件\n"
	case "Write":
		if path != "" {
			return "正在编写 " + path + "\n"
		}
		return "正在编写文件\n"
	case "Edit", "MultiEdit", "NotebookEdit":
		if path != "" {
			return "正在修改 " + path + "\n"
		}
		return "正在修改文件\n"
	case "Grep", "Glob":
		if pattern != "" && path != "" {
			return "正在搜索 " + pattern + "（" + path + "）\n"
		}
		if pattern != "" {
			return "正在搜索 " + pattern + "\n"
		}
		return "正在搜索项目\n"
	case "LS":
		if path != "" {
			return "正在列出目录 " + path + "\n"
		}
		return "正在列出目录\n"
	case "Bash", "PowerShell":
		if command != "" {
			return "正在执行命令 " + truncateForLog(command, 140) + "\n"
		}
		return "正在执行命令\n"
	case "Task":
		description := firstString(input, "description", "prompt")
		if description != "" {
			return "正在处理子任务 " + truncateForLog(description, 100) + "\n"
		}
		return "正在处理子任务\n"
	case "AskUserQuestion":
		return "等待用户确认\n"
	case "ExitPlanMode", "EnterPlanMode":
		plan := firstString(input, "plan")
		if plan != "" {
			return "计划内容：\n" + truncateForLog(plan, 2000) + "\n"
		}
		return "正在生成计划…\n"
	case "TodoWrite":
		if todos, ok := input["todos"].([]interface{}); ok && len(todos) > 0 {
			return fmt.Sprintf("任务列表（%d 项）\n", len(todos))
		}
		return "正在更新任务列表…\n"
	default:
		return ""
	}
}

func firstString(input map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := input[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncateForLog(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "..."
}

func extractSimpleJSONStringField(line, key string) string {
	prefix := `"` + key + `":"`
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return ""
	}
	start := idx + len(prefix)
	end := start
	for end < len(line) {
		if line[end] == '"' && line[end-1] != '\\' {
			break
		}
		end++
	}
	if end <= start || end >= len(line) {
		return ""
	}
	return line[start:end]
}
