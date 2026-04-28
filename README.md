# DeepSeek Code Panel

本地 AI 编程助手桌面应用。基于 Wails v2 + Go + React 构建，调用 `claude` CLI 连接 DeepSeek Anthropic API。

## 环境要求

- **Go** 1.21+
- **Node.js** 18+
- **Wails v2** (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`)
- **claude CLI**（`npm install -g @anthropic-ai/claude-code`）
- **DeepSeek API Key**（[platform.deepseek.com](https://platform.deepseek.com)）

## 快速开始

```bash
# 检查 claude CLI
claude --version

# 安装依赖
cd deepseek-code-panel/frontend && npm install && cd ..

# 开发模式（热重载）
wails dev

# 构建 exe
wails build
```

构建产物在 `build/bin/deepseek-code-panel.exe`。

## 配置

1. 点击左侧「设置」或右上角 `•••` 打开设置面板
2. 填入 DeepSeek API Key
3. Base URL 默认 `https://api.deepseek.com/anthropic`
4. 选择模型：DeepSeek V4 Pro / V4 Flash / Custom

## 使用方式

### 选择项目
- 点击左侧项目行右侧 `↗` 按钮 → 通过系统对话框选择项目目录
- 或点击左侧历史记录 → 自动将该记录的项目设为当前项目并开启新线程

### 运行任务
1. 确保已选择项目目录且已填写 API Key
2. 在底部输入框输入 Prompt
3. 选择权限模式（默认 / 计划 / 接受编辑 / 自动 / 完全访问）
4. 点击 `↑` 提交或按快捷键

### 线程管理
- **单次点击**历史记录 → 以该记录的项目路径开启新线程
- **双击**历史记录 → 查看该线程的历史对话
- 点击项目行 → 在当前项目中新建线程
- 点击「新对话」→ 清空当前会话

### 停止任务
- 点击右上角红色「停止」按钮

## 权限模式

| 模式 | 说明 |
|------|------|
| 默认权限 | 标准权限确认 |
| 计划模式 | 仅执行计划，不修改文件 |
| 接受编辑 | 自动批准编辑操作 |
| 自动权限 | 自动批准常见操作 |
| 完全访问 | 跳过所有权限检查 |

## 日志

运行日志保存在 `%AppData%/deepseek-code-panel/logs/`：

| 文件 | 内容 |
|------|------|
| `runs.db` | SQLite 数据库，所有运行记录 |
| `app.log` | 应用运行日志（含崩溃堆栈） |

每个项目的 `.claude-tools/` 目录下也会保留该项目的运行记录。

崩溃时界面会显示日志路径，将 `app.log` 发送给开发者分析。

## 架构

```
frontend/src/App.tsx   — React UI（全局状态、Markdown 渲染、事件流）
app.go                 — Wails 绑定（项目选择、启动/停止、日志查询）
internal/runner/       — claude CLI 进程管理、stream-json 解析
internal/logstore/     — SQLite 日志持久化
```

## 限制

1. API Key 仅保存在内存中，关闭应用后需重新输入
2. 同一时间只能运行一个任务
3. 仅支持 Windows 平台（WebView2）
4. stream-json 解析聚焦文本内容，不完整解析所有字段
