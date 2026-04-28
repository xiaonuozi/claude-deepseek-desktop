package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"deepseek-code-panel/internal/logstore"
	"deepseek-code-panel/internal/runner"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the main application struct. Its methods are bound to the frontend.
type App struct {
	ctx    context.Context
	runner *runner.Runner
}

// NewApp creates a new App.
func NewApp() *App {
	return &App{
		runner: runner.NewRunner(),
	}
}

// startup is called when the app starts.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Initialize log store
	if err := logstore.Init(); err != nil {
		runtime.LogError(ctx, "Failed to initialize log store: "+err.Error())
	}
}

// CheckClaudeInstalled verifies that the claude CLI is available.
func (a *App) CheckClaudeInstalled() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("未检测到 claude CLI，请先安装并确认 claude --version 可用")
	}
	return string(out), nil
}

// SelectProjectDirectory opens a native directory picker dialog.
func (a *App) SelectProjectDirectory() (string, error) {
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "选择项目目录",
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

func appLogDir() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法获取配置目录: %w", err)
	}
	dir := filepath.Join(cfgDir, "deepseek-code-panel", "logs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("无法创建日志目录: %w", err)
	}
	return dir, nil
}

// WriteAppLog appends a timestamped line to the application log file.
func (a *App) WriteAppLog(message string) error {
	dir, err := appLogDir()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "app.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), strings.TrimSpace(message))
	_, err = f.WriteString(line)
	return err
}

// GetLogPath returns the directory where application logs are stored.
func (a *App) GetLogPath() (string, error) {
	dir, err := appLogDir()
	if err != nil {
		return "", err
	}
	return dir, nil
}

// StartRun validates the request and starts a claude run.
func (a *App) StartRun(req runner.RunRequest) (string, error) {
	req.ProjectPath = strings.TrimSpace(req.ProjectPath)
	req.ThreadID = strings.TrimSpace(req.ThreadID)
	req.ClaudeSessionID = strings.TrimSpace(req.ClaudeSessionID)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.Model = strings.TrimSpace(req.Model)
	req.PermissionMode = strings.TrimSpace(req.PermissionMode)
	req.Language = strings.TrimSpace(req.Language)

	// Validate
	if req.ProjectPath == "" {
		return "", fmt.Errorf("请先选择项目目录")
	}
	if req.APIKey == "" {
		return "", fmt.Errorf("请输入 DeepSeek API Key")
	}
	if req.Prompt == "" {
		return "", fmt.Errorf("请输入 Prompt")
	}
	if req.BaseURL == "" {
		req.BaseURL = "https://api.deepseek.com/anthropic"
	}
	if req.Model == "" {
		req.Model = "deepseek-v4-pro"
	}
	if req.PermissionMode == "" {
		req.PermissionMode = "default"
	}
	if req.Language == "" {
		req.Language = "中文"
	}

	if a.runner.IsRunning() {
		return "", fmt.Errorf("已有任务正在运行")
	}

	runID := uuid.New().String()
	if req.ThreadID == "" {
		req.ThreadID = runID
	}
	a.runner.Start(a.ctx, req, runID)
	return runID, nil
}

// StopRun stops the currently running claude process.
func (a *App) StopRun() error {
	return a.runner.Stop(a.ctx)
}

// GetRecentLogs returns the most recent run logs from the JSONL file.
func (a *App) GetRecentLogs(limit int) ([]logstore.LogEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	return logstore.GetRecentLogs(limit)
}

// GetThreadLogs returns all runs in one conversation thread.
func (a *App) GetThreadLogs(threadID string) ([]logstore.LogEntry, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return []logstore.LogEntry{}, nil
	}
	return logstore.GetThreadLogs(threadID)
}
