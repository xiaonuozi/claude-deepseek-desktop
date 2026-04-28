# DeepSeek Code Panel

本地 AI 编程助手桌面应用。基于 Wails v2 + Go + React 构建，调用 `claude` CLI 连接 DeepSeek Anthropic API。

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
4. 选择模型：DeepSeek V4 Pro / V4 Flash / 自定义

配置会自动保存，下次打开无需重新输入。

### 选择项目

- 点击左侧项目行右侧 **↗** → 系统对话框选择项目目录
- 或点击左侧历史记录 → 自动切换至该项目并加载历史对话

### 开始对话

1. 确认已选择项目目录且已填写 API Key
2. 在底部输入框输入任务描述
3. 选择权限模式（见下方说明）
4. 点击 **↑** 或回车提交

支持**多任务并发**——一个线程运行期间，切换到其他线程可同时提交新任务。

### 权限模式

| 模式 | 说明 |
|------|------|
| 默认权限 | 标准权限确认，每次操作需批准 |
| 计划模式 | 仅执行计划，不修改文件 |
| 接受编辑 | 自动批准编辑操作 |
| 自动权限 | 自动批准常见操作 |
| 完全访问 | 跳过所有权限检查 |

### 线程管理

| 操作 | 效果 |
|------|------|
| 单击历史记录 | 切换至该线程，查看历史对话 |
| 点击「新对话」 | 创建空白新线程 |
| 点击项目行 | 在当前项目中新建线程 |
| hover × | 删除线程（含确认对话框） |

历史列表默认显示最近 **5 条**，超过可点击「展开更多」。

### 输出视图

- **输出视图**（`▱`）：格式化显示
  - Markdown 渲染、代码高亮
  - 工具调用带彩色标签（`Read` `Write` `Edit` `Bash` 等），一目了然
  - 工具返回结果可折叠展开（`查看结果`）
  - 深度思考内容可折叠，默认收起
  - 运行结束时显示耗时和 token 用量（例如 `运行结束 · exit=0 · 耗时 12s · 输入 1523 token · 输出 487 token`）
- **Raw 视图**（`⌗`）：完整 stream-json 原始输出

### 停止任务

点击右上角红色「停止」按钮，仅停止当前线程的任务，不影响其他线程。

### 注意事项

如果你自己的项目要开源，建议在 `.gitignore` 中添加：

```
.claude-tools/
```

避免把对话记录提交到项目仓库。

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

### 构建

```bash
wails build
```

产物在 `build/bin/deepseek-code-panel.exe`。

### 架构

```
frontend/src/App.tsx   — React UI（状态管理、Markdown/思考块/工具调用渲染、事件流）
app.go                 — Wails 绑定（项目选择、启停、日志查询、线程删除、设置读写）
internal/runner/       — claude CLI 进程管理、stream-json 解析（thinking/tool/usage）、panic 恢复、多任务并发调度
internal/logstore/     — SQLite 持久化（运行记录 + 设置）
```

### 日志

运行日志保存在 `%AppData%/deepseek-code-panel/logs/`：

| 文件 | 内容 |
|------|------|
| `runs.db` | SQLite 数据库（运行记录 + 设置） |
| `app.log` | 应用运行日志（含崩溃堆栈） |

每个项目的 `.claude-tools/` 目录下也会保留该项目专属的运行记录。

### 隐私与安全

- API Key 保存在本地 SQLite 数据库中（`%AppData%` 下），不会随源码开源泄露
- 源代码中无硬编码密钥
- 子进程环境变量仅对 claude CLI 可见，应用退出后消失
