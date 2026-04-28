import { Component, ReactNode, useState, useEffect, useRef, useCallback } from 'react';
import { EventsOn, EventsOff, LogPrint } from '../wailsjs/runtime/runtime';
import {
  CheckClaudeInstalled,
  SelectProjectDirectory,
  StartRun,
  StopRun,
  GetRecentLogs,
  GetThreadLogs,
  DeleteThreads,
  WriteAppLog,
  GetLogPath,
  SaveSetting,
  LoadSetting,
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
  meta?: Record<string, any>;
};

type LogEntry = logstore.LogEntry;
type ViewMode = 'output' | 'raw';
type OutputSegment = {
  type: string;
  text: string;
  meta?: Record<string, any>;
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

function toFiniteNumber(value: any): number {
  const numberValue = Number(value);
  return Number.isFinite(numberValue) ? numberValue : NaN;
}

function formatDurationMS(ms: any): string {
  const durationMS = toFiniteNumber(ms);
  if (!Number.isFinite(durationMS)) return '';
  const wholeMS = Math.max(0, Math.round(durationMS));
  if (wholeMS < 1000) return `${wholeMS}ms`;
  const totalSeconds = Math.floor(wholeMS / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}m${seconds}s`;
}

function formatTokenNumber(value: any): string {
  const tokenCount = toFiniteNumber(value);
  if (!Number.isFinite(tokenCount) || tokenCount <= 0) return '0';
  return Math.round(tokenCount).toLocaleString('en-US');
}

function buildRunStats(durationMS: any, inputTokens: any, outputTokens: any): string {
  const duration = formatDurationMS(durationMS);
  const input = toFiniteNumber(inputTokens);
  const output = toFiniteNumber(outputTokens);
  const hasTokens = (Number.isFinite(input) && input > 0) || (Number.isFinite(output) && output > 0);
  const parts: string[] = [];
  if (duration) parts.push(`耗时: ${duration}`);
  parts.push(hasTokens ? `token: 输入 ${formatTokenNumber(input)} / 输出 ${formatTokenNumber(output)}` : 'token: 未记录');
  return parts.join(' / ');
}

function buildRunMetaText(model: string, permissionMode: string, createdAt: string, durationMS: any, inputTokens: any, outputTokens: any): string {
  return `model: ${model} / mode: ${permissionMode} / time: ${createdAt} / ${buildRunStats(durationMS, inputTokens, outputTokens)}\n`;
}

function attachRunStats(text: string, stats: string): string {
  const base = text.trim().replace(/\s+\/\s+(duration|耗时):.*$/u, '');
  return `${base} / ${stats}\n`;
}

function mergeDoneStats(lines: OutputLine[], event: OutputLine): OutputLine[] {
  const stats = buildRunStats(event.meta?.duration_ms, event.meta?.input_tokens, event.meta?.output_tokens);
  let updated = false;
  const next = lines.map((line) => {
    if (!updated && line.run_id === event.run_id && (line.type === 'status' || line.type === 'system')) {
      updated = true;
      return {
        ...line,
        text: attachRunStats(line.text, stats),
        meta: { ...(line.meta || {}), ...(event.meta || {}) },
      };
    }
    return line;
  });
  if (updated) return next;
  return [...next, { ...event, type: 'status', text: `${stats}\n` }];
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
  let thinkingBuf = '';
  let inThinking = false;

  const flushMarkdown = () => {
    if (markdown.trim()) {
      segments.push({ type: 'display', text: markdown });
    }
    markdown = '';
  };

  const flushThinking = () => {
    if (inThinking && thinkingBuf.trim()) {
      segments.push({ type: 'thinking-block', text: thinkingBuf.trim() });
    }
    thinkingBuf = '';
    inThinking = false;
  };

  for (const line of lines) {
    if (line.type === 'thinking-delta') {
      inThinking = true;
      thinkingBuf += line.text;
      continue;
    }
    if (line.type === 'thinking-start') {
      inThinking = true;
      continue;
    }
    if (line.type === 'thinking-end') {
      flushThinking();
      continue;
    }
    if (line.type === 'display') {
      markdown += line.text;
      continue;
    }
    flushMarkdown();
    flushThinking();
    if (line.type === 'tool-start' || line.type === 'tool-end') {
      segments.push({ type: line.type, text: line.text, meta: line.meta });
    } else {
      segments.push({ type: line.type, text: line.text });
    }
  }

  flushMarkdown();
  flushThinking();
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
  const [permissionMode, setPermissionMode] = useState('bypassPermissions');
  const [projectPath, setProjectPath] = useState('');
  const [language, setLanguage] = useState('中文');
  const [prompt, setPrompt] = useState('');

  const [isRunning, setIsRunning] = useState(false);
  const [activeRunID, setActiveRunID] = useState('');
  const [outputLines, setOutputLines] = useState<OutputLine[]>([]);
  const [rawOutput, setRawOutput] = useState<string[]>([]);
  const [viewMode, setViewMode] = useState<ViewMode>('output');
  const [error, setError] = useState('');
  const [claudeVersion, setClaudeVersion] = useState('');
  const [showSettings, setShowSettings] = useState(false);
  const [logPath, setLogPath] = useState('');
  const [showAllThreads, setShowAllThreads] = useState(false);
  const [showProjectThreads, setShowProjectThreads] = useState(true);

  const [recentLogs, setRecentLogs] = useState<LogEntry[]>([]);
  const [activeThreadID, setActiveThreadID] = useState('');
  const [activeSessionID, setActiveSessionID] = useState('');

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
    LoadSetting('api_key').then((v) => { if (v) setApiKey(v); }).catch(() => {});
    LoadSetting('base_url').then((v) => { if (v) setBaseUrl(v); }).catch(() => {});
  }, [loadRecentLogs]);

  // Persist settings on change
  const saveApiKey = useCallback((key: string) => {
    setApiKey(key);
    SaveSetting('api_key', key).catch(() => {});
  }, []);
  const saveBaseUrl = useCallback((url: string) => {
    setBaseUrl(url);
    SaveSetting('base_url', url).catch(() => {});
  }, []);

  useEffect(() => {
    EventsOn('run-event', (event: OutputLine) => {
      try {
        if (event.raw) {
          setRawOutput((prev) => [
            ...prev,
            event.type === 'stderr' ? '[STDERR] ' + event.raw : event.raw!,
          ]);
        }

        if (event.type === 'display' || event.type === 'status' || event.type === 'stderr' ||
          event.type === 'thinking-start' || event.type === 'thinking-delta' || event.type === 'thinking-end' ||
          event.type === 'tool-start' || event.type === 'tool-end' || event.type === 'tool-result') {
          setOutputLines((prev) => [...prev, event]);
        }

        if (event.type === 'done') {
          setOutputLines((prev) => mergeDoneStats(prev, event));
          const exitCode = toFiniteNumber(event.meta?.exit_code);
          if (Number.isFinite(exitCode) && exitCode !== 0) {
            setError(`运行退出码: ${exitCode}`);
          }
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
  const visibleLogs = projectPath
    ? recentLogs.filter((log) => !log.project_path || log.project_path === projectPath)
    : recentLogs;
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
        setShowProjectThreads(true);
        setActiveThreadID(createLocalID('new'));
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
    setActiveThreadID(createLocalID('new'));
  };

  const handleToggleProjectThreads = () => {
    setShowProjectThreads((current) => !current);
  };

  const handleProjectThread = async () => {
    if (!projectPath) {
      await handleSelectDir();
      return;
    }
    setOutputLines([]);
    setRawOutput([]);
    setError('');
    setActiveThreadID(createLocalID('new'));
    setActiveSessionID('');
    setPrompt('');
    setShowProjectThreads(true);
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
      setActiveThreadID(createLocalID('new'));
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

  const handleDeleteThread = async (logEntry: LogEntry, event: React.MouseEvent) => {
    event.stopPropagation();
    const threadID = logEntry.thread_id || logEntry.id;
    if (!window.confirm(`确定删除此线程的所有记录？\n\n${logEntry.prompt?.slice(0, 60) || '(empty)'}`)) return;
    try {
      await DeleteThreads(threadID);
      log('INFO', 'handleDeleteThread: deleted thread ' + threadID);
      if ((activeThreadID || activeSessionID) && activeThreadID === threadID) {
        setActiveThreadID('');
        setActiveSessionID('');
        setOutputLines([]);
        setRawOutput([]);
        setViewMode('output');
      }
      await loadRecentLogs();
    } catch (err: any) {
      log('ERROR', 'handleDeleteThread error: ' + String(err));
      setError('删除失败: ' + err);
    }
  };

  const doStartRun = async (mode: string) => {
    const trimmedPrompt = prompt.trim();

    setError('');
    if (!projectPath) { setError('请先选择项目目录'); return; }
    if (!apiKey.trim()) { setError('请输入 DeepSeek API Key'); setShowSettings(true); return; }
    if (!trimmedPrompt) { setError('请输入任务内容'); return; }
    const threadID = (activeThreadID && !activeThreadID.startsWith('new-')) ? activeThreadID : createLocalID('thread');
    const runID = createLocalID('run');

    setActiveRunID(runID);
    setActiveThreadID(threadID);

    log('INFO', `doStartRun: thread=${threadID} run=${runID} project=${projectPath} model=${effectiveModel} mode=${mode}`);

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
      await StopRun(activeRunID);
      setIsRunning(false);
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
      {
        type: 'system',
        text: buildRunMetaText(entry.model, entry.permission_mode, entry.created_at, entry.duration_ms, entry.input_tokens, entry.output_tokens),
        run_id: entry.id,
        thread_id: threadID,
        meta: {
          exit_code: entry.exit_code,
          duration_ms: entry.duration_ms,
          input_tokens: entry.input_tokens,
          output_tokens: entry.output_tokens,
        },
      },
      { type: 'display', text: entry.display_output || '(empty)\n', run_id: entry.id, thread_id: threadID },
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

          <div className="project-row-wrap">
            <button className="project-row" onClick={handleToggleProjectThreads} title={showProjectThreads ? '收起线程' : '展开线程'}>
              <span className="project-toggle">{showProjectThreads ? '▾' : '▸'}</span>
              <span className="folder-icon">▱</span>
              <span className="project-name">{projectName}</span>
            </button>
            <button className="project-new-thread" onClick={handleProjectThread} disabled={isRunning} title={projectPath ? '开启新线程' : '选择项目目录'}>+</button>
          </div>
        </section>

        {showProjectThreads && <section className="sidebar-group history-group">
          <div className="run-list">
            {visibleLogs.length === 0 ? (
              <div className="empty-list">暂无聊天</div>
            ) : (
              (showAllThreads ? visibleLogs : visibleLogs.slice(0, 5)).map((log) => (
                <button
                  key={log.id}
                  className={`history-row ${(log.thread_id || log.id) === activeThreadID ? 'active' : ''}`}
                  onClick={() => handleViewLog(log)}
                >
                  <span>{truncate(log.prompt || '(empty prompt)', 28)}</span>
                  <time>{formatAge(log.created_at)}</time>
                  <span className="delete-thread" onClick={(e) => handleDeleteThread(log, e)} title="删除线程">×</span>
                </button>
              ))
            )}
            {visibleLogs.length > 5 && (
              <button className="expand-toggle" onClick={() => setShowAllThreads(!showAllThreads)}>
                {showAllThreads ? '收起' : `展开更多 (${visibleLogs.length - 5})`}
              </button>
            )}
          </div>
        </section>}

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
                  {outputSegments.map((segment, index) => {
                    if (segment.type === 'display') {
                      return <MarkdownMessage key={`md-${index}`} text={segment.text} />;
                    }
                    if (segment.type === 'user') {
                      return <div key={`user-${index}`} className="user-message">{segment.text}</div>;
                    }
                    if (segment.type === 'thinking-block') {
                      return (
                        <details key={`think-${index}`} className="thinking-fold">
                          <summary>深度思考内容</summary>
                          <pre className="thinking-body">{segment.text}</pre>
                        </details>
                      );
                    }
                    if (segment.type === 'tool-start') {
                      const toolName = segment.meta?.name || '';
                      return (
                        <div key={`tool-${index}`} className="output-line tool-line tool-start-line">
                          <span className="tool-badge">{toolName}</span>
                          <span className="tool-text">{segment.text}</span>
                        </div>
                      );
                    }
                    if (segment.type === 'tool-end') {
                      const toolName = segment.meta?.name || '';
                      return (
                        <div key={`tool-${index}`} className="output-line tool-line tool-end-line">
                          <span className="tool-badge end">{toolName}</span>
                          <span className="tool-text">{segment.text}</span>
                        </div>
                      );
                    }
                    if (segment.type === 'tool-result') {
                      return (
                        <details key={`tr-${index}`} className="tool-result-fold">
                          <summary>查看结果</summary>
                          <pre className="tool-result-body">{segment.text}</pre>
                        </details>
                      );
                    }
                    if (segment.type === 'done') {
                      return (
                        <div key={`done-${index}`} className="output-line done-line">{segment.text}</div>
                      );
                    }
                    return (
                      <pre key={`line-${index}`} className={`output-line ${segment.type}`}>
                        {segment.text}
                      </pre>
                    );
                  })}
                </div>
              </article>
            )}
          </div>
        </div>

        <section className="composer-dock">
          <div className="composer-card">
            <textarea
              value={prompt}
              onChange={(event) => setPrompt(event.target.value)}
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
                onChange={(event) => saveApiKey(event.target.value)}
                placeholder="sk-..."
                disabled={isRunning}
              />
            </label>

            <label className="field">
              <span>Base URL</span>
              <input
                type="text"
                value={baseUrl}
                onChange={(event) => saveBaseUrl(event.target.value)}
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
