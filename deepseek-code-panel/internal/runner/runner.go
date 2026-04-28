package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"deepseek-code-panel/internal/logstore"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Runner manages a single claude CLI process.
type Runner struct {
	mu      sync.Mutex
	running bool
	runID   string
	cancel  context.CancelFunc
}

// NewRunner creates a new Runner.
func NewRunner() *Runner {
	return &Runner{}
}

// IsRunning returns whether a run is currently in progress.
func (r *Runner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Start launches the claude CLI with the given request and streams output via Wails events.
// ctx must be the Wails app context (from a.startup).
func (r *Runner) Start(ctx context.Context, req RunRequest, runID string) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		runtime.EventsEmit(ctx, "run-event", RunEvent{
			Type:     "error",
			Text:     "已有任务正在运行",
			RunID:    runID,
			ThreadID: req.ThreadID,
		})
		return
	}
	r.running = true
	r.runID = runID
	r.mu.Unlock()

	go r.run(ctx, req, runID)
}

// Stop terminates the currently running claude process.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return fmt.Errorf("没有正在运行的任务")
	}
	if r.cancel != nil {
		r.cancel()
	}
	return nil
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
		r.running = false
		r.cancel = nil
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
	r.cancel = cancel
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
		"start: thread=%s session=%s model=%s permission=%s prompt=%q",
		req.ThreadID,
		req.ClaudeSessionID,
		req.Model,
		req.PermissionMode,
		truncateForLog(req.Prompt, 120),
	)); err != nil {
		runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
			Type:     "stderr",
			Text:     "写入项目启动日志失败: " + err.Error(),
			RunID:    runID,
			ThreadID: req.ThreadID,
		})
	}

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

	// Collect output for logging
	var displayParts []string
	var rawParts []string
	var outputMu sync.Mutex

	// Emit start event
	runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
		Type:            "status",
		Text:            runStartText(req),
		RunID:           runID,
		ThreadID:        req.ThreadID,
		ClaudeSessionID: req.ClaudeSessionID,
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
			outputMu.Lock()
			rawParts = append(rawParts, line)
			outputMu.Unlock()

			// Extract user-visible text or compact status from stream-json.
			eventType, display := parser.extract(line)
			now := time.Now().Format(time.RFC3339Nano)

			if display != "" {
				if eventType == "display" {
					outputMu.Lock()
					displayParts = append(displayParts, display)
					outputMu.Unlock()
				}
				runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
					Type:            eventType,
					Text:            display,
					Raw:             line,
					RunID:           runID,
					ThreadID:        req.ThreadID,
					ClaudeSessionID: parser.sessionID,
					Timestamp:       now,
				})
			} else {
				// Emit as raw only (for raw output view)
				runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
					Type:            "stdout",
					Text:            line,
					Raw:             line,
					RunID:           runID,
					ThreadID:        req.ThreadID,
					ClaudeSessionID: parser.sessionID,
					Timestamp:       now,
				})
			}
		}
	}()

	// Read stderr line by line
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			rawParts = append(rawParts, "[STDERR] "+line)
			outputMu.Unlock()
			runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
				Type:            "stderr",
				Text:            line,
				Raw:             line,
				RunID:           runID,
				ThreadID:        req.ThreadID,
				ClaudeSessionID: parser.sessionID,
				Timestamp:       time.Now().Format(time.RFC3339Nano),
			})
		}
	}()

	wg.Wait()

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
	displayOutput := strings.Join(displayParts, "")
	rawOutput := strings.Join(rawParts, "\n")
	outputMu.Unlock()
	claudeSessionID := parser.sessionID
	if claudeSessionID == "" {
		claudeSessionID = req.ClaudeSessionID
	}

	// Emit done event
	runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
		Type:            "done",
		Text:            fmt.Sprintf("\n<<< 运行结束，exit_code=%d, 耗时 %d ms\n", exitCode, durationMS),
		RunID:           runID,
		ThreadID:        req.ThreadID,
		ClaudeSessionID: claudeSessionID,
	})

	// Save log (API key is NOT included)
	log := RunLog{
		ID:              runID,
		ThreadID:        req.ThreadID,
		ClaudeSessionID: claudeSessionID,
		CreatedAt:       startTime.Format(time.RFC3339),
		ProjectPath:     req.ProjectPath,
		Model:           req.Model,
		PermissionMode:  req.PermissionMode,
		Prompt:          req.Prompt, // Only original prompt, not language prefix
		DisplayOutput:   displayOutput,
		RawOutput:       rawOutput,
		ExitCode:        exitCode,
		DurationMS:      durationMS,
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
	}); err != nil {
		runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
			Type:            "stderr",
			Text:            "保存日志失败: " + err.Error(),
			RunID:           runID,
			ThreadID:        req.ThreadID,
			ClaudeSessionID: claudeSessionID,
		})
	}
}

type streamParser struct {
	tools           map[int]*toolTrace
	thinking        map[int]bool
	lastMessageText string
	sessionID       string
}

type toolTrace struct {
	name  string
	input strings.Builder
}

func newStreamParser() *streamParser {
	return &streamParser{
		tools:    map[int]*toolTrace{},
		thinking: map[int]bool{},
	}
}

// extract turns Claude stream-json into either markdown display text or compact status text.
func (p *streamParser) extract(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}

	// Quick check: if not JSON, return as-is.
	if !strings.HasPrefix(line, "{") {
		return "display", line + "\n"
	}

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return "display", line + "\n"
	}

	eventType, _ := obj["type"].(string)
	if sessionID, ok := obj["session_id"].(string); ok && sessionID != "" {
		p.sessionID = sessionID
	}
	idx := blockIndex(obj)

	// 1. content_block_delta: {"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}
	if delta, ok := obj["delta"].(map[string]interface{}); ok {
		if text, ok := delta["text"].(string); ok && text != "" {
			p.lastMessageText += text
			return "display", text
		}
		if partial, ok := delta["partial_json"].(string); ok && partial != "" {
			if tool := p.tools[idx]; tool != nil {
				tool.input.WriteString(partial)
			}
			return "", ""
		}
	}

	if eventType == "content_block_start" {
		if cb, ok := obj["content_block"].(map[string]interface{}); ok {
			cbType, _ := cb["type"].(string)
			if cbType == "tool_use" {
				name, _ := cb["name"].(string)
				tool := &toolTrace{name: name}
				if input, ok := cb["input"].(map[string]interface{}); ok {
					if b, err := json.Marshal(input); err == nil {
						tool.input.Write(b)
					}
				}
				p.tools[idx] = tool
				return "", ""
			}
			if cbType == "thinking" || cbType == "redacted_thinking" {
				p.thinking[idx] = true
				return "status", "正在深度思考...\n"
			}
		}
	}

	if eventType == "content_block_stop" {
		if tool := p.tools[idx]; tool != nil {
			delete(p.tools, idx)
			if status := describeToolUse(tool.name, tool.input.String()); status != "" {
				return "status", status
			}
			return "", ""
		}
		if p.thinking[idx] {
			delete(p.thinking, idx)
			return "status", "深度思考即将结束。\n"
		}
	}

	// 2. assistant message with content array. Tool calls are intentionally ignored here.
	if msg, ok := obj["message"].(map[string]interface{}); ok {
		if contentArr, ok := msg["content"].([]interface{}); ok {
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
				if text == p.lastMessageText {
					return "", ""
				}
				if strings.HasPrefix(text, p.lastMessageText) {
					suffix := strings.TrimPrefix(text, p.lastMessageText)
					p.lastMessageText = text
					return "display", suffix
				}
				p.lastMessageText = text
				return "display", text
			}
		}
	}

	// 3. direct result field
	if result, ok := obj["result"].(string); ok && result != "" {
		if result == p.lastMessageText {
			return "", ""
		}
		return "display", result
	}

	// 4. error messages
	if eventType == "error" || eventType == "aborted" {
		if errMsg, ok := obj["error"].(string); ok && errMsg != "" {
			return "error", "[Error: " + errMsg + "]\n"
		}
		return "error", "[运行被中断]\n"
	}

	// Could not extract display text
	return "", ""
}

func runStartText(req RunRequest) string {
	if req.ClaudeSessionID != "" {
		return fmt.Sprintf("继续线程：%s / %s，prompt %d 字符\n", req.Model, req.PermissionMode, len([]rune(req.Prompt)))
	}
	return fmt.Sprintf("新建线程：%s / %s，prompt %d 字符\n", req.Model, req.PermissionMode, len([]rune(req.Prompt)))
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
