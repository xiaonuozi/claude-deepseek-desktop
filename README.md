# Claude Code DeepSeek GUI - DeepSeek Code Panel

## Who is this for?

DeepSeek Code Panel is a Windows desktop GUI for **Claude Code CLI** that connects to **DeepSeek Anthropic API**. It provides streaming output, multi-session conversations, project workspaces, SQLite history, Claude session resume, tool-call rendering, and permission modes such as plan, accept edits, auto permissions, and full access.
This project is for developers who want to use **DeepSeek models** with **Claude Code CLI** in a desktop GUI instead of a pure terminal workflow.

It is especially useful if you need:

- A Windows desktop panel for Claude Code
- DeepSeek Anthropic API integration
- Project-based coding sessions
- Streaming output and raw `stream-json` view
- Multi-thread conversations
- Resume previous Claude sessions
- Permission modes such as plan, accept edits, auto permissions, and full access

---

## 使用指南

### 安装

从 [Releases](../../releases) 下载 `deepseek-code-panel.exe`，直接运行即可。

前置依赖：系统需安装 [claude CLI](https://docs.anthropic.com/en/docs/claude-code)：

```bash
npm install -g @anthropic-ai/claude-code
claude --version
```

### 首次配置

1. 打开应用，点击左下角「设置」
2. 填入 **DeepSeek API Key**（在 [platform.deepseek.com](https://platform.deepseek.com) 获取）
3. Base URL 默认为 `https://api.deepseek.com/anthropic`，一般无需修改
4. 选择模型：**DeepSeek V4 Pro** / **V4 Flash** / **自定义模型**（输入任意模型名）
5. 设置 **Language**（默认 `中文`）：每次提交任务时会自动在 Prompt 前追加语言指令

配置会自动保存到本地 SQLite 数据库，下次打开无需重新输入。

### 选择项目

- 点击左侧项目行右侧 **↗** → 系统对话框选择项目目录
- 或点击左侧历史记录 → 自动切换至该项目并加载历史对话
- 选择项目后，左侧历史列表自动筛选当前项目下的线程

### 开始对话

1. 确认已选择项目目录且已填写 API Key
2. 在底部输入框输入任务描述
3. 选择权限模式（见下方说明）
4. 点击 **↑** 或回车提交

支持**多任务并发**——一个线程运行期间，切换到其他线程可同时提交新任务。

### 权限模式

| 模式     | 说明                         |
| -------- | ---------------------------- |
| 默认权限 | 标准权限确认，每次操作需批准 |
| 计划模式 | 仅执行计划，不修改文件       |
| 接受编辑 | 自动批准编辑操作             |
| 自动权限 | 自动批准常见操作             |
| 完全访问 | 跳过所有权限检查             |

启动后的默认权限模式为 **完全访问**，可在底部下拉菜单中切换。

### 线程管理

| 操作               | 效果                         |
| ------------------ | ---------------------------- |
| 单击历史记录       | 切换至该线程，查看历史对话   |
| 点击「新对话」     | 创建空白新线程               |
| 点击项目行         | 展开或收起该项目下的线程列表 |
| 点击项目行右侧 `+` | 在当前项目中开启新线程       |
| hover ×            | 删除线程（含确认对话框）     |

- 历史列表按项目筛选，只显示当前项目下的线程
- 空白新线程不会写入历史，直到输入内容并提交运行后才会出现
- 历史列表默认显示最近 **5 条**，超过可点击「展开更多」

### 输出视图

- **格式化视图**（`▱`）：
  - Markdown 渲染、代码高亮
  - 工具调用带彩色标签（`Read` `Write` `Edit` `Bash` `Grep` 等）
  - 工具返回结果可折叠展开（`查看结果`）
  - 深度思考内容可折叠，默认收起
  - 在 model/mode/time 行显示耗时和 token 用量（例如 `耗时: 12s / token: 输入 1,523 / 输出 487`）
- **Raw 视图**（`⌗`）：完整 stream-json 原始输出

### 停止任务

点击右上角红色「停止」按钮，仅停止当前线程的任务，不影响其他线程。

### 会话续接

同一线程的下一次任务会自动使用 `--resume` 续接之前的 Claude 会话，保留上下文记忆。

### 注意事项

如果你自己的项目要开源，建议在 `.gitignore` 中添加：

```
.claude-tools/
```

避免把对话记录提交到项目仓库。

---

## 环境变量（调试与诊断）

以下环境变量用于开发调试或问题排查，正常使用无需设置：

| 变量                         | 说明                                                                                             |
| ---------------------------- | ------------------------------------------------------------------------------------------------ |
| `CLAUDE_TOOLS_RAW_LOG=1`     | 将完整的 stream-json 原始输出写入磁盘文件（位于项目 `.claude-tools/` 目录下）                    |
| `CLAUDE_TOOLS_DIAGNOSTICS=1` | 启用诊断日志（同时也会启用 Raw 日志），记录每个阶段的字节量、截断信息等，用于排查 OOM 或性能问题 |

---

## 开发者指南

### 环境要求

- **Go** 1.21+
- **Node.js** 18+
- **Wails v2**：`go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **claude CLI**：`npm install -g @anthropic-ai/claude-code`

### 开发模式

```bash
cd deepseek-code-panel
cd frontend && npm install && cd ..

# 启动开发模式（前端热重载 + Go 自动编译）
wails dev
```

开发模式下建议设置环境变量启用诊断日志：

```bash
# PowerShell
$env:CLAUDE_TOOLS_DIAGNOSTICS = "1"; wails dev

# Bash
CLAUDE_TOOLS_DIAGNOSTICS=1 wails dev
```

### 构建

```bash
wails build
```

产物在 `build/bin/deepseek-code-panel.exe`。

### 架构

```
frontend/src/App.tsx    — React UI（状态管理、Markdown/思考块/工具调用渲染、事件流、项目级线程筛选）
frontend/src/style.css  — 全部样式
app.go                  — Wails 绑定（项目选择、启停、日志查询、线程删除、设置读写、应用日志）
command_windows.go      — Windows 平台子进程配置（隐藏窗口）
command_other.go        — 其他平台子进程配置
internal/runner/        — claude CLI 进程管理、stream-json 流式解析（thinking/tool/usage/token）、
                          panic 恢复、并发控制、语言注入、内存限流（cappedTextBuffer）、诊断日志
internal/logstore/      — SQLite 持久化（运行记录 + 设置）、Schema 自动迁移、旧 JSONL 迁移、Token 回填
```

### 数据存储

- **全局存储**：`%AppData%/deepseek-code-panel/logs/runs.db`（SQLite）
  - 表 `runs`：所有运行记录（线程、session、模型、权限、prompt、输出、token 用量、耗时）
  - 表 `settings`：用户设置（API Key、Base URL、Language 等键值对）
- **项目存储**：每个项目的 `.claude-tools/` 目录下同步保存 SQLite 数据库和运行日志
- 旧版本 JSONL 格式（`runs.jsonl`）在启动时自动迁移至 SQLite
- Token 用量在迁移时自动从原始输出中提取并回填

### 日志文件

| 文件         | 内容                             |
| ------------ | -------------------------------- |
| `runs.db`    | SQLite 数据库（运行记录 + 设置） |
| `app.log`    | 应用运行日志（含崩溃堆栈）       |
| `runs.jsonl` | 旧版 JSONL 格式（迁移后保留）    |

### 内存管理

- 展示文本（Markdown）的预览上限为 **2MB**，Raw 输出的预览上限为 **5MB**，超出部分在内存中截断
- 原始流日志默认不写磁盘，仅在设置 `CLAUDE_TOOLS_RAW_LOG=1` 后输出
- 前端事件的单次推送有独立的字符上限，防止 UI 卡顿
- 通过 `CLAUDE_TOOLS_DIAGNOSTICS=1` 可监控各阶段的字节量和截断情况，帮助排查 OOM

### 隐私与安全

- API Key 保存在本地 SQLite 数据库中（`%AppData%` 下），不会随源码开源泄露
- 源代码中无硬编码密钥
- 子进程环境变量仅对 claude CLI 可见，应用退出后消失
