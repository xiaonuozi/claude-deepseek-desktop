import { Component, ReactNode, useState, useEffect, useRef, useCallback } from 'react';
import { EventsOn, EventsOff, LogPrint } from '../wailsjs/runtime/runtime';
import {
  CheckClaudeInstalled,
  SelectProjectDirectory,
  StartRun,
  StopRun,
  GetRecentLogs,
  GetThreadLogs,
  WriteAppLog,
  GetLogPath,
} from '../wailsjs/go/main/App';
import { logstore } from '../wailsjs/go/models';

type OutputLine = {
  type: string;
  text: string;
  raw?: string;
  run_id: string;
  thread_id?: string;
  claude_session_id?: string;
  timestamp?: string;
};

type LogEntry = logstore.LogEntry;
type ViewMode = 'output' | 'raw';
type OutputSegment = {
  type: string;
  text: string;
};

class AppErrorBoundary extends Component<{ children: ReactNode }, { message: string }> {
  state = { message: '' };

  static getDerivedStateFromError(error: Error) {
    return { message: error?.message || String(error) };
  }

  render() {
    if (this.state.message) {
      return (
        <div className="fatal-screen">
          <strong>界面发生错误</strong>
          <pre>{this.state.message}</pre>
        </div>
      );
    }
    return this.props.children;
  }
}

function createLocalID(prefix: string) {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

const MODELS = [
  { value: 'deepseek-v4-pro', label: 'DeepSeek V4 Pro' },
  { value: 'deepseek-v4-flash', label: 'DeepSeek V4 Flash' },
];

const PERMISSION_MODES = [
  { value: 'default', label: '默认权限' },
  { value: 'plan', label: '计划模式' },
  { value: 'acceptEdits', label: '接受编辑' },
  { value: 'auto', label: '自动权限' },
  { value: 'bypassPermissions', label: '完全访问权限' },
];

function buildOutputSegments(lines: OutputLine[]): OutputSegment[] {
  const segments: OutputSegment[] = [];
  let markdown = '';

  const flushMarkdown = () => {
    if (markdown.trim()) {
      segments.push({ type: 'display', text: markdown });
    }
    markdown = '';
  };

  for (const line of lines) {
    if (line.type === 'display') {
      markdown += line.text;
      continue;
    }
    flushMarkdown();
    segments.push({ type: line.type, text: line.text });
  }

  flushMarkdown();
  return segments;
}

type MarkdownBlock =
  | { type: 'paragraph'; text: string }
  | { type: 'heading'; level: number; text: string }
  | { type: 'code'; language: string; text: string }
  | { type: 'ul'; items: string[] }
  | { type: 'ol'; items: string[] }
  | { type: 'quote'; text: string };

function parseMarkdown(text: string): MarkdownBlock[] {
  const lines = text.replace(/\r\n/g, '\n').split('\n');
  const blocks: MarkdownBlock[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    if (!line.trim()) {
      i += 1;
      continue;
    }

    const fence = line.match(/^```(\w+)?\s*$/);
    if (fence) {
      const language = fence[1] || '';
      const code: string[] = [];
      i += 1;
      while (i < lines.length && !lines[i].startsWith('```')) {
        code.push(lines[i]);
        i += 1;
      }
      if (i < lines.length) i += 1;
      blocks.push({ type: 'code', language, text: code.join('\n') });
      continue;
    }

    const heading = line.match(/^(#{1,3})\s+(.+)$/);
    if (heading) {
      blocks.push({ type: 'heading', level: heading[1].length, text: heading[2] });
      i += 1;
      continue;
    }

    if (/^\s*[-*]\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*[-*]\s+/, ''));
        i += 1;
      }
      blocks.push({ type: 'ul', items });
      continue;
    }

    if (/^\s*\d+\.\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*\d+\.\s+/, ''));
        i += 1;
      }
      blocks.push({ type: 'ol', items });
      continue;
    }

    if (/^\s*>\s?/.test(line)) {
      const quote: string[] = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        quote.push(lines[i].replace(/^\s*>\s?/, ''));
        i += 1;
      }
      blocks.push({ type: 'quote', text: quote.join('\n') });
      continue;
    }

    const paragraph: string[] = [];
    while (
      i < lines.length &&
      lines[i].trim() &&
      !/^```/.test(lines[i]) &&
      !/^(#{1,3})\s+/.test(lines[i]) &&
      !/^\s*[-*]\s+/.test(lines[i]) &&
      !/^\s*\d+\.\s+/.test(lines[i]) &&
      !/^\s*>\s?/.test(lines[i])
    ) {
      paragraph.push(lines[i]);
      i += 1;
    }
    blocks.push({ type: 'paragraph', text: paragraph.join('\n') });
  }

  return blocks;
}

function renderInline(text: string, keyPrefix: string): Array<string | JSX.Element> {
  const parts = text.split(/(`[^`]+`|\*\*[^*]+\*\*)/g);
  return parts.map((part, index) => {
    if (part.startsWith('`') && part.endsWith('`')) {
      return <code key={`${keyPrefix}-code-${index}`}>{part.slice(1, -1)}</code>;
    }
    if (part.startsWith('**') && part.endsWith('**')) {
      return <strong key={`${keyPrefix}-strong-${index}`}>{part.slice(2, -2)}</strong>;
    }
    return part;
  });
}

function MarkdownMessage({ text }: { text: string }) {
  const blocks = parseMarkdown(text);

  return (
    <div className="markdown-body">
      {blocks.map((block, index) => {
        if (block.type === 'heading') {
          const Tag = (`h${block.level}` as keyof JSX.IntrinsicElements);
          return <Tag key={index}>{renderInline(block.text, `h-${index}`)}</Tag>;
        }
        if (block.type === 'code') {
          return (
            <pre key={index} className="markdown-code">
              {block.language && <span className="code-language">{block.language}</span>}
              <code>{block.text}</code>
            </pre>
          );
        }
        if (block.type === 'ul') {
          return <ul key={index}>{block.items.map((item, itemIndex) => <li key={itemIndex}>{renderInline(item, `ul-${index}-${itemIndex}`)}</li>)}</ul>;
        }
        if (block.type === 'ol') {
          return <ol key={index}>{block.items.map((item, itemIndex) => <li key={itemIndex}>{renderInline(item, `ol-${index}-${itemIndex}`)}</li>)}</ol>;
        }
        if (block.type === 'quote') {
          return <blockquote key={index}>{renderInline(block.text, `q-${index}`)}</blockquote>;
        }
        return <p key={index}>{renderInline(block.text, `p-${index}`)}</p>;
      })}
    </div>
  );
}

function App() {
  const [apiKey, setApiKey] = useState('');
  const [baseUrl, setBaseUrl] = useState('https://api.deepseek.com/anthropic');
  const [model, setModel] = useState('deepseek-v4-pro');
  const [customModel, setCustomModel] = useState('');
  const [permissionMode, setPermissionMode] = useState('default');
  const [projectPath, setProjectPath] = useState('');
  const [language, setLanguage] = useState('中文');
  const [prompt, setPrompt] = useState('');

  const [isRunning, setIsRunning] = useState(false);
  const [outputLines, setOutputLines] = useState<OutputLine[]>([]);
  const [rawOutput, setRawOutput] = useState<string[]>([]);
  const [viewMode, setViewMode] = useState<ViewMode>('output');
  const [error, setError] = useState('');
  const [claudeVersion, setClaudeVersion] = useState('');
  const [showSettings, setShowSettings] = useState(false);
  const [logPath, setLogPath] = useState('');

  const [recentLogs, setRecentLogs] = useState<LogEntry[]>([]);
  const [activeThreadID, setActiveThreadID] = useState('');
  const [activeSessionID, setActiveSessionID] = useState('');
  const [isComposing, setIsComposing] = useState(false);

  const outputRef = useRef<HTMLDivElement>(null);

  const log = useCallback((level: string, msg: string) => {
    const line = `[${level}] ${msg}`;
    try { LogPrint(line); } catch {}
    try { WriteAppLog(line); } catch {}
  }, []);

  const loadRecentLogs = useCallback(async () => {
    try {
      const logs = await GetRecentLogs(30);
      setRecentLogs(logs || []);
      setProjectPath((current) => current || logs?.[0]?.project_path || '');
    } catch {
      // Keep the chat usable even if the local log file cannot be read.
    }
  }, []);

  useEffect(() => {
    CheckClaudeInstalled()
      .then((ver) => setClaudeVersion(ver.trim()))
      .catch((err) => setError('claude CLI 未检测到: ' + err));
    loadRecentLogs();
    GetLogPath()
      .then((p) => setLogPath(p))
      .catch(() => {});
  }, [loadRecentLogs]);

  useEffect(() => {
    EventsOn('run-event', (event: OutputLine) => {
      try {
        if (event.raw) {
          setRawOutput((prev) => [
            ...prev,
            event.type === 'stderr' ? '[STDERR] ' + event.raw : event.raw!,
          ]);
        }

      if (event.type === 'display' || event.type === 'status' || event.type === 'stderr' || event.type === 'done') {
        setOutputLines((prev) => [...prev, event]);
      }

        if (event.type === 'error') {
          log('ERROR', 'run-event error: ' + event.text);
          setError(event.text);
        }

        if (event.thread_id) {
          setActiveThreadID(event.thread_id);
        }

        if (event.claude_session_id) {
          setActiveSessionID(event.claude_session_id);
        }

        if (event.type === 'done') {
          log('INFO', 'run-event done');
          setIsRunning(false);
          loadRecentLogs();
        }
      } catch (err: any) {
        log('ERROR', 'run-event handler error: ' + String(err));
      }
    });

    return () => {
      EventsOff('run-event');
    };
  }, [loadRecentLogs, log]);

  useEffect(() => {
    if (outputRef.current) {
      outputRef.current.scrollTop = outputRef.current.scrollHeight;
    }
  }, [outputLines, rawOutput, viewMode]);

  const truncate = (value: string, size: number) => (
    value && value.length > size ? value.slice(0, size - 1) + '…' : value
  );

  const effectiveModel = customModel.trim() || model;
  const selectedModelLabel = MODELS.find((item) => item.value === model)?.label || effectiveModel;
  const selectedPermissionLabel = PERMISSION_MODES.find((item) => item.value === permissionMode)?.label || permissionMode;
  const projectName = projectPath ? projectPath.split(/[\\/]/).filter(Boolean).pop() || projectPath : 'Claude Tools';
  const activeLog = activeThreadID
    ? recentLogs.find((log) => (log.thread_id || log.id) === activeThreadID)
    : null;
  const outputSegments = buildOutputSegments(outputLines);
  const conversationTitle = activeLog
    ? truncate(activeLog.prompt || '历史运行', 26)
    : activeThreadID
      ? '新线程'
    : prompt.trim()
      ? truncate(prompt.trim(), 26)
      : '新对话';

  const handleSelectDir = async () => {
    log('INFO', 'handleSelectDir: opening directory picker');
    try {
      const dir = await SelectProjectDirectory();
      if (dir) {
        log('INFO', 'handleSelectDir: selected ' + dir);
        setProjectPath(dir);
        setActiveThreadID('');
        setActiveSessionID('');
        setOutputLines([]);
        setRawOutput([]);
        setPrompt('');
        setError('');
        setViewMode('output');
      }
    } catch (err: any) {
      const msg = '选择目录失败: ' + err;
      log('ERROR', msg);
      setError(msg);
    }
  };

  const handleClear = () => {
    setOutputLines([]);
    setRawOutput([]);
    setError('');
    setActiveThreadID('');
    setActiveSessionID('');
    setViewMode('output');
  };

  const handleNewTask = () => {
    handleClear();
    setPrompt('');
  };

  const handleProjectThread = async () => {
    if (!projectPath) {
      setError('请先点击项目右侧的 ↗ 选择项目目录');
      return;
    }
    setOutputLines([]);
    setRawOutput([]);
    setError('');
    setActiveThreadID('');
    setActiveSessionID('');
    setPrompt('');
    setViewMode('output');
  };

  const handleNewThreadFromLog = (logEntry: LogEntry) => {
    try {
      const threadProjectPath = logEntry.project_path;
      log('INFO', 'handleNewThreadFromLog: project=' + threadProjectPath + ' id=' + logEntry.id);
      if (!threadProjectPath) {
        setError('该历史记录没有项目路径，请使用右上角按钮选择项目');
        return;
      }
      setProjectPath(threadProjectPath);
      setActiveThreadID('');
      setActiveSessionID('');
      setOutputLines([]);
      setRawOutput([]);
      setPrompt('');
      setError('');
      setViewMode('output');
    } catch (err: any) {
      log('ERROR', 'handleNewThreadFromLog error: ' + String(err));
    }
  };

  const doStartRun = async (mode: string) => {
    const trimmedPrompt = prompt.trim();

    setError('');
    if (!projectPath) { setError('请先选择项目目录'); return; }
    if (!apiKey.trim()) { setError('请输入 DeepSeek API Key'); setShowSettings(true); return; }
    if (!trimmedPrompt) { setError('请输入任务内容'); return; }
    if (isRunning) { setError('已有任务正在运行'); return; }

    const threadID = activeThreadID || createLocalID('thread');
    const runID = createLocalID('run');

    log('INFO', `doStartRun: thread=${threadID} run=${runID} project=${projectPath} model=${effectiveModel} mode=${mode}`);

    setActiveThreadID(threadID);
    setOutputLines((prev) => [
      ...prev,
      { type: 'user', text: trimmedPrompt, run_id: runID, thread_id: threadID },
    ]);
    if (!activeThreadID) {
      setRawOutput([]);
    }
    setViewMode('output');
    setIsRunning(true);
    setPrompt('');

    try {
      await StartRun({
        project_path: projectPath,
        thread_id: threadID,
        claude_session_id: activeSessionID,
        prompt: trimmedPrompt,
        api_key: apiKey.trim(),
        base_url: baseUrl.trim(),
        model: effectiveModel,
        permission_mode: mode,
        language: language.trim(),
      });
      log('INFO', `doStartRun: StartRun() returned OK`);
    } catch (err: any) {
      const msg = '启动失败: ' + err;
      log('ERROR', msg);
      setError(msg);
      setIsRunning(false);
    }
  };

  const handleStop = async () => {
    try {
      await StopRun();
    } catch (err: any) {
      setError('停止失败: ' + err);
    }
  };

  const handleViewLog = async (log: LogEntry) => {
    const threadID = log.thread_id || log.id;
    const logs = await GetThreadLogs(threadID);
    const threadLogs = logs && logs.length ? logs : [log];
    const lastLog = threadLogs[threadLogs.length - 1];

    setActiveThreadID(threadID);
    setActiveSessionID(lastLog.claude_session_id || '');
    setProjectPath(lastLog.project_path || log.project_path);
    setViewMode('output');
    setRawOutput(threadLogs.flatMap((entry) => [
      `--- run ${entry.id.slice(0, 8)} / ${entry.created_at} ---`,
      ...(entry.raw_output ? entry.raw_output.split('\n') : []),
    ]));
    setError(lastLog.exit_code === 0 ? '' : `历史运行退出码: ${lastLog.exit_code}`);
    setOutputLines(threadLogs.flatMap((entry) => [
      { type: 'user', text: entry.prompt, run_id: entry.id, thread_id: threadID },
      { type: 'system', text: `model: ${entry.model} / mode: ${entry.permission_mode} / time: ${entry.created_at}\n`, run_id: entry.id, thread_id: threadID },
      { type: 'display', text: entry.display_output || '(empty)\n', run_id: entry.id, thread_id: threadID },
      { type: 'done', text: `\nexit_code=${entry.exit_code}, duration=${entry.duration_ms} ms\n`, run_id: entry.id, thread_id: threadID },
    ]));
  };

  const formatTime = (iso: string) => {
    try {
      return new Date(iso).toLocaleString('zh-CN', {
        hour: '2-digit',
        minute: '2-digit',
      });
    } catch {
      return iso;
    }
  };

  const formatAge = (iso: string) => {
    const time = new Date(iso).getTime();
    if (Number.isNaN(time)) return '';
    const minutes = Math.max(1, Math.round((Date.now() - time) / 60000));
    if (minutes < 60) return `${minutes} 分`;
    const hours = Math.round(minutes / 60);
    if (hours < 24) return `${hours} 小时`;
    return `${Math.round(hours / 24)} 天`;
  };

  return (
    <div id="App">
      <aside className="codex-sidebar">
        <nav className="sidebar-nav">
          <button onClick={handleNewTask} disabled={isRunning}><span>□</span>新对话</button>
          <button><span>⌕</span>搜索</button>
          <button><span>⌘</span>插件</button>
          <button><span>◷</span>自动化</button>
        </nav>

        <section className="sidebar-group project-group">
          <div className="group-heading">
            <span>项目</span>
            <div className="group-actions">
              <button onClick={handleSelectDir} disabled={isRunning} title="选择项目目录">↗</button>
              <button onClick={handleClear} disabled={isRunning} title="清空输出">≡</button>
            </div>
          </div>

          <button className="project-row" onClick={handleProjectThread} disabled={isRunning} title={projectPath ? '在该项目中新建线程' : '选择项目目录'}>
            <span className="folder-icon">▱</span>
            <span>{projectName}</span>
          </button>
        </section>

        <section className="sidebar-group history-group">
          <div className="run-list">
            {recentLogs.length === 0 ? (
              <div className="empty-list">暂无聊天</div>
            ) : (
              recentLogs.map((log) => (
                <button
                  key={log.id}
                  className={`history-row ${(log.thread_id || log.id) === activeThreadID ? 'active' : ''}`}
                  onClick={() => handleNewThreadFromLog(log)}
                  onDoubleClick={() => handleViewLog(log)}
                >
                  <span>{truncate(log.prompt || '(empty prompt)', 28)}</span>
                  <time>{formatAge(log.created_at)}</time>
                </button>
              ))
            )}
          </div>
        </section>

        <button className="settings-entry" onClick={() => setShowSettings(true)}>
          <span>⚙</span>设置
        </button>
      </aside>

      <main className="codex-main">
        <header className="chat-header">
          <div className="chat-title">
            <strong>{conversationTitle}</strong>
            <span>Claude Tools</span>
            <button className="icon-button" onClick={() => setShowSettings(true)} title="更多设置">•••</button>
          </div>

          <div className="header-actions">
            {isRunning ? (
              <button className="pill-button danger" onClick={handleStop}>停止</button>
            ) : (
              <button className="icon-button" onClick={() => doStartRun(permissionMode)} title="运行">▷</button>
            )}
            <button className="model-chip" onClick={() => setShowSettings(true)} title={effectiveModel}>
              <span className="chip-logo">◆</span>
              <span>{selectedModelLabel}</span>
            </button>
            <button className="pill-button" onClick={() => doStartRun(permissionMode)} disabled={isRunning}>
              提交⌄
            </button>
            <span className="divider" />
            <button className={viewMode === 'output' ? 'icon-button active' : 'icon-button'} onClick={() => setViewMode('output')} title="输出">▱</button>
            <button className={viewMode === 'raw' ? 'icon-button active' : 'icon-button'} onClick={() => setViewMode('raw')} title="Raw">⌗</button>
            <span className="run-stat ok">+{rawOutput.length}</span>
            <span className="run-stat bad">-{outputLines.filter((line) => line.type === 'stderr' || line.type === 'error').length}</span>
          </div>
        </header>

        <div className="conversation-scroll" ref={outputRef}>
          <div className="conversation-lane">
            {error && <div className="error-banner">{error}{logPath ? <><br/><small>日志路径: {logPath}</small></> : ''}</div>}

            {viewMode === 'raw' ? (
              <pre className="raw-block">{rawOutput.length ? rawOutput.join('\n') : '(empty)'}</pre>
            ) : outputLines.length === 0 && !error ? (
              <div className="empty-thread">
                <div className="empty-title">选择项目后开始运行</div>
                <div className="empty-subtitle">{projectPath || '左侧选择项目目录，然后在下方输入任务'}</div>
              </div>
            ) : (
              <article className="assistant-card">
                <div className="message-heading">
                  <strong>{conversationTitle}</strong>
                  <span>{formatTime(new Date().toISOString())}</span>
                </div>
                <div className="response-stream">
                  {outputSegments.map((segment, index) => (
                    segment.type === 'display' ? (
                      <MarkdownMessage key={`md-${index}`} text={segment.text} />
                    ) : segment.type === 'user' ? (
                      <div key={`user-${index}`} className="user-message">{segment.text}</div>
                    ) : (
                      <pre key={`line-${index}`} className={`output-line ${segment.type}`}>
                        {segment.text}
                      </pre>
                    )
                  ))}
                </div>
              </article>
            )}
          </div>
        </div>

        <section className="composer-dock">
          <div className="composer-card">
            <textarea
              value={prompt}
              onChange={(event) => {
                try {
                  if (!isComposing) setPrompt(event.target.value);
                } catch (err: any) {
                  log('ERROR', 'textarea onChange: ' + String(err));
                }
              }}
              onCompositionStart={() => { try { setIsComposing(true); } catch (err: any) { log('ERROR', 'compositionStart: ' + String(err)); } }}
              onCompositionEnd={(event) => {
                try {
                  setIsComposing(false);
                  setPrompt((event.target as HTMLTextAreaElement).value);
                } catch (err: any) {
                  log('ERROR', 'compositionEnd: ' + String(err));
                }
              }}
              placeholder="要求后续变更"
              disabled={isRunning}
            />

            <div className="composer-bar">
              <div className="composer-left">
                <button className="round-button" onClick={handleSelectDir} disabled={isRunning} title="选择项目">+</button>
                <select value={permissionMode} onChange={(event) => setPermissionMode(event.target.value)} disabled={isRunning}>
                  {PERMISSION_MODES.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}
                </select>
              </div>

              <div className="composer-right">
                <select value={model} onChange={(event) => { setModel(event.target.value); setCustomModel(''); }} disabled={isRunning}>
                  {MODELS.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}
                  <option value="__custom__">Custom model</option>
                </select>
                <button className="send-button" onClick={() => doStartRun(permissionMode)} disabled={isRunning} title="提交">↑</button>
              </div>
            </div>
          </div>

          <div className="workspace-status">
            <button>▱ 本地模式⌄</button>
            <button>⌁ master⌄</button>
            <span>{claudeVersion || 'Claude CLI'}</span>
            <span>{selectedPermissionLabel}</span>
          </div>
        </section>

        {showSettings && (
          <aside className="settings-sheet">
            <div className="settings-head">
              <strong>设置</strong>
              <button onClick={() => setShowSettings(false)}>×</button>
            </div>

            <label className="field">
              <span>API Key</span>
              <input
                type="password"
                value={apiKey}
                onChange={(event) => setApiKey(event.target.value)}
                placeholder="sk-..."
                disabled={isRunning}
              />
            </label>

            <label className="field">
              <span>Base URL</span>
              <input
                type="text"
                value={baseUrl}
                onChange={(event) => setBaseUrl(event.target.value)}
                disabled={isRunning}
              />
            </label>

            {model === '__custom__' && (
              <label className="field">
                <span>Model name</span>
                <input
                  type="text"
                  value={customModel}
                  onChange={(event) => setCustomModel(event.target.value)}
                  placeholder="deepseek-v4-pro"
                  disabled={isRunning}
                />
              </label>
            )}

            <label className="field">
              <span>Language</span>
              <input
                type="text"
                value={language}
                onChange={(event) => setLanguage(event.target.value)}
                disabled={isRunning}
              />
            </label>

            <div className="settings-meta">
              <div><span>Project</span><strong title={projectPath}>{projectName}</strong></div>
              <div><span>Raw lines</span><strong>{rawOutput.length}</strong></div>
              <div><span>Status</span><strong>{isRunning ? 'Running' : 'Idle'}</strong></div>
            </div>
          </aside>
        )}
      </main>
    </div>
  );
}

export default function AppShell() {
  return (
    <AppErrorBoundary>
      <App />
    </AppErrorBoundary>
  );
}
