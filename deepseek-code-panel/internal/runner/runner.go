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

const (
	maxFrontendEventRawChars  = 12000
	maxFrontendEventTextChars = 80000
	frontendTruncatedNote     = "\n[内容过长，前端事件已截断；完整内容已写入本地日志]\n"
)

// Runner manages concurrent claude CLI processes.
type Runner struct {
	mu   sync.Mutex
	runs map[string]context.CancelFunc // runID → cancel
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
		Text:            runStartText(req, startTime),
		RunID:           runID,
		ThreadID:        req.ThreadID,
		ClaudeSessionID: req.ClaudeSessionID,
		Timestamp:       startTime.Format(time.RFC3339),
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
			meta := parser.meta()
			now := time.Now().Format(time.RFC3339Nano)

			if display != "" {
				if eventType == "display" {
					outputMu.Lock()
					displayParts = append(displayParts, display)
					outputMu.Unlock()
				}
				frontendDisplay := truncateFrontendPayload(display, maxFrontendEventTextChars)
				runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
					Type:            eventType,
					Text:            frontendDisplay,
					Raw:             truncateFrontendPayload(line, maxFrontendEventRawChars),
					RunID:           runID,
					ThreadID:        req.ThreadID,
					ClaudeSessionID: parser.sessionID,
					Timestamp:       now,
					Meta:            meta,
				})
			} else {
				runtime.EventsEmit(wailsCtx, "run-event", RunEvent{
					Type:            "stdout",
					Text:            truncateFrontendPayload(line, maxFrontendEventTextChars),
					Raw:             truncateFrontendPayload(line, maxFrontendEventRawChars),
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
				Text:            truncateFrontendPayload(line, maxFrontendEventTextChars),
				Raw:             truncateFrontendPayload(line, maxFrontendEventRawChars),
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

	// Collect token usage from parser.
	inputTokens, outputTokens := parser.usage()

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
		},
	})
}

type streamParser struct {
	tools           map[int]*toolTrace
	thinking        map[int]bool
	thinkingText    map[int]strings.Builder
	lastMessageText string
	sessionID       string
	lastMeta        map[string]interface{}
	inputTokens     int
	outputTokens    int
	usageByMessage  map[string]tokenUsage
}

type toolTrace struct {
	name  string
	input strings.Builder
}

type tokenUsage struct {
	input  int
	output int
}

func newStreamParser() *streamParser {
	return &streamParser{
		tools:          map[int]*toolTrace{},
		thinking:       map[int]bool{},
		thinkingText:   map[int]strings.Builder{},
		usageByMessage: map[string]tokenUsage{},
	}
}

func (p *streamParser) usage() (int, int) {
	if len(p.usageByMessage) > 0 {
		inputTokens := 0
		outputTokens := 0
		for _, usage := range p.usageByMessage {
			inputTokens += usage.input
			outputTokens += usage.output
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
		p.lastMessageText = ""
	}
	idx := blockIndex(obj)

	// Handle deltas
	if delta, ok := obj["delta"].(map[string]interface{}); ok {
		deltaType, _ := delta["type"].(string)

		if text, ok := delta["text"].(string); ok && text != "" {
			p.lastMessageText += text
			return "display", text
		}

		if deltaType == "thinking_delta" {
			if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
				if tb, exists := p.thinkingText[idx]; exists {
					tb.WriteString(thinking)
				}
				return "thinking-delta", thinking
			}
		}

		if deltaType == "input_json_delta" {
			if partial, ok := delta["partial_json"].(string); ok && partial != "" {
				if tool := p.tools[idx]; tool != nil {
					tool.input.WriteString(partial)
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
				tool := &toolTrace{name: name}
				if input, ok := cb["input"].(map[string]interface{}); ok {
					if b, err := json.Marshal(input); err == nil {
						tool.input.Write(b)
					}
				}
				p.tools[idx] = tool
				p.lastMeta = map[string]interface{}{"phase": "start", "name": name}
				return "tool-start", describeToolStart(name)
			}
			if cbType == "thinking" || cbType == "redacted_thinking" {
				p.thinking[idx] = true
				p.thinkingText[idx] = strings.Builder{}
				p.lastMeta = map[string]interface{}{"phase": "start"}
				return "thinking-start", "深度思考中…\n"
			}
		}
	}

	if eventType == "content_block_stop" {
		if tool := p.tools[idx]; tool != nil {
			delete(p.tools, idx)
			p.lastMeta = map[string]interface{}{
				"phase": "end",
				"name":  tool.name,
				"input": tool.input.String(),
			}
			if status := describeToolUse(tool.name, tool.input.String()); status != "" {
				return "tool-end", status
			}
			return "", ""
		}
		if p.thinking[idx] {
			delete(p.thinking, idx)
			thinkingContent := ""
			if tb, exists := p.thinkingText[idx]; exists {
				thinkingContent = tb.String()
				delete(p.thinkingText, idx)
			}
			p.lastMeta = map[string]interface{}{
				"phase":   "end",
				"content": thinkingContent,
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

	if result, ok := obj["result"].(string); ok && result != "" {
		if result == p.lastMessageText {
			return "", ""
		}
		return "display", result
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
