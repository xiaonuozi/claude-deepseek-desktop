# DeepSeek Code Panel

本地 AI 编程助手控制台桌面应用。通过 Wails + Go + React 构建，调用本地 `claude` CLI 连接 DeepSeek Anthropic API。

## 环境要求

- **Go** 1.21+
- **Node.js** 18+
- **Wails v2** (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`)
- **claude CLI**（Anthropic 官方 CLI 工具）
- **DeepSeek API Key**（在 [DeepSeek 平台](https://platform.deepseek.com) 获取）

## 检查 claude CLI

```bash
claude --version
```

如果提示找不到命令，请先安装：
```bash
npm install -g @anthropic-ai/claude-code
```

## 安装依赖

```bash
# 进入项目目录
cd deepseek-code-panel

# 安装前端依赖
cd frontend && npm install && cd ..
```

## 开发模式

```bash
wails dev
```

启动后会自动打开桌面窗口。前端修改会自动热重载，Go 修改会自动重新编译。

## 构建 Windows exe

```bash
wails build
```

构建产物在 `build/bin/deepseek-code-panel.exe`。

## 配置 DeepSeek API Key

1. 在 DeepSeek 平台获取 API Key
2. 在应用右侧配置区输入 API Key
3. Base URL 默认为 `https://api.deepseek.com/anthropic`
4. 选择模型（deepseek-v4-pro / deepseek-v4-flash）

## 使用方式

1. 点击"选择项目目录"选择一个本地项目目录
2. 输入 DeepSeek API Key
3. 选择模型和权限模式
4. 输入 Prompt
5. 点击"运行"开始

- **运行**：使用当前选择的权限模式执行
- **计划模式运行**：强制使用 `plan` 权限模式
- **全权限运行**：强制使用 `bypassPermissions` 权限模式
- **停止**：终止当前正在运行的 claude 进程

## 日志

运行记录保存在以下位置（不包含 API Key）：

- Windows: `%AppData%/deepseek-code-panel/logs/runs.jsonl`

## 当前 MVP 限制

1. API Key 仅保存在内存中，关闭应用后需重新输入
2. 同一时间只能运行一个任务
3. 不支持同时选择多个项目
4. 无用户认证/登录功能
5. 日志使用 JSONL 文件存储，无数据库
6. stream-json 解析仅提取文本内容，不完整解析所有字段
7. 仅支持单行 stream-json 解析，不支持 SSH 模式输出
