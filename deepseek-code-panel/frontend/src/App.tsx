import { Component, ReactNode, useState, useEffect, useRef, useCallback, useMemo } from 'react';
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
type ActivityItem = {
  type: string;
  text: string;
  meta?: Record<string, any>;
};
type OutputSegment = {
  type: string;
  text: string;
  meta?: Record<string, any>;
  items?: ActivityItem[];
};
type ThreadBuffers = {
  outputLines: OutputLine[];
  rawOutput: string[];
  error: string;
  sessionID: string;
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

const MAX_RAW_LINES = 160;
const MAX_RAW_CHARS = 120_000;
const MAX_OUTPUT_SEGMENTS = 1200;
const MAX_DISPLAY_CHARS = 500_000;
const MAX_THINKING_CHARS = 80_000;
const MAX_TOOL_RESULT_CHARS = 120_000;
const SCROLL_BOTTOM_THRESHOLD = 120;
const TRUNCATED_NOTE = '\n\n[内容过长，界面已截断]\n';

function compactText(value: string, max = 96): string {
  const text = value.replace(/\s+/g, ' ').trim();
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

function appendLimitedText(current: string, addition: string, limit: number): string {
  if (!addition) return current;
  if (current.includes(TRUNCATED_NOTE) || current.length >= limit) return current;
  const next = current + addition;
  if (next.length <= limit) return next;
  return next.slice(0, Math.max(0, limit - TRUNCATED_NOTE.length)) + TRUNCATED_NOTE;
}

function limitEventText(event: OutputLine): OutputLine {
  if (event.type === 'tool-result' && event.text.length > MAX_TOOL_RESULT_CHARS) {
    return { ...event, text: event.text.slice(0, MAX_TOOL_RESULT_CHARS - TRUNCATED_NOTE.length) + TRUNCATED_NOTE };
  }
  if (event.type === 'thinking-delta' && event.text.length > MAX_THINKING_CHARS) {
    return { ...event, text: event.text.slice(0, MAX_THINKING_CHARS - TRUNCATED_NOTE.length) + TRUNCATED_NOTE };
  }
  if (event.type === 'display' && event.text.length > MAX_DISPLAY_CHARS) {
    return { ...event, text: event.text.slice(0, MAX_DISPLAY_CHARS - TRUNCATED_NOTE.length) + TRUNCATED_NOTE };
  }
  return event;
}

function shouldMergeOutputLine(prev: OutputLine | undefined, next: OutputLine): boolean {
  if (!prev) return false;
  if (prev.run_id !== next.run_id || prev.thread_id !== next.thread_id || prev.type !== next.type) return false;
  return next.type === 'display' || next.type === 'thinking-delta' || next.type === 'stderr';
}

function appendOutputLine(lines: OutputLine[], event: OutputLine): OutputLine[] {
  const limitedEvent = limitEventText(event);
  const prev = lines[lines.length - 1];
  let nextLines: OutputLine[];
  if (shouldMergeOutputLine(prev, limitedEvent)) {
    const limit = limitedEvent.type === 'thinking-delta' ? MAX_THINKING_CHARS : MAX_DISPLAY_CHARS;
    nextLines = [
      ...lines.slice(0, -1),
      { ...prev!, text: appendLimitedText(prev!.text, limitedEvent.text, limit) },
    ];
  } else {
    nextLines = [...lines, limitedEvent];
  }
  if (nextLines.length <= MAX_OUTPUT_SEGMENTS) return nextLines;
  return nextLines.slice(nextLines.length - MAX_OUTPUT_SEGMENTS);
}

function appendRawLine(lines: string[], line: string): string[] {
  const next = [...lines, line];
  let start = next.length;
  let chars = 0;
  while (start > 0 && next.length - start < MAX_RAW_LINES) {
    const candidate = next[start - 1] || '';
    if (chars + candidate.length > MAX_RAW_CHARS) break;
    chars += candidate.length;
    start -= 1;
  }
  if (start <= 0) return next;
  return [`[Raw 输出过长，已隐藏前 ${start} 行]`, ...next.slice(start)];
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

function isActivityType(type: string): boolean {
  return type === 'status' || type === 'system' || type === 'done' ||
    type === 'thinking-start' || type === 'thinking-delta' || type === 'thinking-end' ||
    type === 'thinking-block' || type === 'tool-start' || type === 'tool-end' || type === 'tool-result';
}

function sameActivityItem(a: ActivityItem | undefined, b: ActivityItem): boolean {
  if (!a) return false;
  return a.type === b.type && a.text === b.text && a.meta?.name === b.meta?.name;
}

function buildActivitySummary(items: ActivityItem[]): string {
  const status = [...items].reverse().find((item) => item.type === 'status' || item.type === 'system');
  if (status?.meta?.duration_ms !== undefined) {
    return `已处理 · ${buildRunStats(status.meta.duration_ms, status.meta.input_tokens, status.meta.output_tokens)}`;
  }
  const latest = [...items].reverse().find((item) => item.type !== 'status' && item.type !== 'system' && item.type !== 'thinking-block' && item.text.trim());
  if (latest) {
    return `处理中 · ${compactText(latest.text)}`;
  }
  return '处理中';
}

function buildOutputSegments(lines: OutputLine[]): OutputSegment[] {
  const segments: OutputSegment[] = [];
  let markdown = '';
  let thinkingBuf = '';
  let inThinking = false;
  let activityItems: ActivityItem[] = [];

  const flushMarkdown = () => {
    if (markdown.trim()) {
      segments.push({ type: 'display', text: markdown });
    }
    markdown = '';
  };

  const flushThinking = () => {
    if (inThinking && thinkingBuf.trim()) {
      appendActivity({ type: 'thinking-block', text: thinkingBuf.trim() });
    }
    thinkingBuf = '';
    inThinking = false;
  };

  const flushActivity = () => {
    if (!activityItems.length) return;
    segments.push({
      type: 'activity',
      text: buildActivitySummary(activityItems),
      items: activityItems,
    });
    activityItems = [];
  };

  const appendActivity = (item: ActivityItem) => {
    if (!item.text.trim() && item.type !== 'status' && item.type !== 'system') return;
    if (sameActivityItem(activityItems[activityItems.length - 1], item)) return;
    activityItems.push(item);
  };

  for (const line of lines) {
    if (line.type === 'thinking-delta') {
      inThinking = true;
      thinkingBuf += line.text;
      continue;
    }
    if (line.type === 'thinking-start') {
      flushMarkdown();
      appendActivity({ type: 'thinking-start', text: line.text || '深度思考中…', meta: line.meta });
      inThinking = true;
      continue;
    }
    if (line.type === 'thinking-end') {
      flushThinking();
      appendActivity({ type: 'thinking-end', text: line.text || '深度思考结束', meta: line.meta });
      continue;
    }
    if (isActivityType(line.type)) {
      flushMarkdown();
      if (line.type === 'tool-start' || line.type === 'tool-end' || line.type === 'tool-result' || line.type === 'status' || line.type === 'system' || line.type === 'done') {
        appendActivity({ type: line.type, text: line.text, meta: line.meta });
      }
      continue;
    }
    if (line.type === 'display') {
      flushThinking();
      flushActivity();
      markdown += line.text;
      continue;
    }
    flushMarkdown();
    flushThinking();
    flushActivity();
    segments.push({ type: line.type, text: line.text, meta: line.meta });
  }

  flushMarkdown();
  flushThinking();
  flushActivity();
  return segments;
}

type MarkdownBlock =
  | { type: 'paragraph'; text: string }
  | { type: 'heading'; level: number; text: string }
  | { type: 'code'; language: string; text: string }
  | { type: 'ul'; items: string[] }
  | { type: 'ol'; items: string[] }
  | { type: 'quote'; text: string }
  | { type: 'table'; headers: string[]; rows: string[][] };

function splitTableRow(line: string): string[] {
  return line.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map((cell) => cell.trim());
}

function isTableSeparator(line: string): boolean {
  const cells = splitTableRow(line);
  return cells.length > 1 && cells.every((cell) => /^:?-{3,}:?$/.test(cell));
}

function looksLikeTableRow(line: string): boolean {
  return line.includes('|') && splitTableRow(line).length > 1;
}

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

    if (looksLikeTableRow(line) && i + 1 < lines.length && isTableSeparator(lines[i + 1])) {
      const headers = splitTableRow(line);
      const rows: string[][] = [];
      i += 2;
      while (i < lines.length && looksLikeTableRow(lines[i]) && lines[i].trim()) {
        rows.push(splitTableRow(lines[i]));
        i += 1;
      }
      blocks.push({ type: 'table', headers, rows });
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
      !(looksLikeTableRow(lines[i]) && i + 1 < lines.length && isTableSeparator(lines[i + 1])) &&
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
        if (block.type === 'table') {
          return (
            <div key={index} className="markdown-table-wrap">
              <table>
                <thead>
                  <tr>{block.headers.map((cell, cellIndex) => <th key={cellIndex}>{renderInline(cell, `th-${index}-${cellIndex}`)}</th>)}</tr>
                </thead>
                <tbody>
                  {block.rows.map((row, rowIndex) => (
                    <tr key={rowIndex}>
                      {block.headers.map((_, cellIndex) => <td key={cellIndex}>{renderInline(row[cellIndex] || '', `td-${index}-${rowIndex}-${cellIndex}`)}</td>)}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          );
        }
        return <p key={index}>{renderInline(block.text, `p-${index}`)}</p>;
      })}
    </div>
  );
}

function FolderIcon() {
  return (
    <svg viewBox="0 0 20 20" aria-hidden="true" focusable="false">
      <path d="M2.5 5.5A2.5 2.5 0 0 1 5 3h3.1c.63 0 1.22.3 1.6.8l.72.95H15A2.5 2.5 0 0 1 17.5 7.25v6.25A2.5 2.5 0 0 1 15 16H5a2.5 2.5 0 0 1-2.5-2.5v-8Z" />
      <path d="M2.9 7h14.2" />
    </svg>
  );
}

function ActivityGroup({ segment }: { segment: OutputSegment }) {
  const items = segment.items || [];
  return (
    <details className="activity-fold">
      <summary>
        <span className={segment.text.startsWith('已处理') ? 'activity-dot done' : 'activity-dot'} />
        <span>{segment.text}</span>
        <small>{items.length} 项</small>
      </summary>
      <div className="activity-list">
        {items.map((item, index) => {
          if (item.type === 'thinking-block') {
            return (
              <details key={`activity-thinking-${index}`} className="thinking-fold">
                <summary>深度思考内容</summary>
                <pre className="thinking-body">{item.text}</pre>
              </details>
            );
          }
          if (item.type === 'tool-result') {
            return (
              <details key={`activity-result-${index}`} className="tool-result-fold">
                <summary>查看工具结果</summary>
                <pre className="tool-result-body">{item.text}</pre>
              </details>
            );
          }
          const toolName = item.meta?.name || (item.type.startsWith('tool-') ? 'TOOL' : '');
          const isDone = item.type === 'tool-end';
          return (
            <div key={`activity-${index}`} className={`activity-item ${item.type}`}>
              {toolName && <span className={isDone ? 'tool-badge end' : 'tool-badge'}>{toolName}</span>}
              <span className="activity-text">{item.text}</span>
            </div>
          );
        })}
      </div>
    </details>
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

  const [outputLines, setOutputLines] = useState<OutputLine[]>([]);
  const [rawOutput, setRawOutput] = useState<string[]>([]);
  const [error, setError] = useState('');
  const [claudeVersion, setClaudeVersion] = useState('');
  const [showSettings, setShowSettings] = useState(false);
  const [logPath, setLogPath] = useState('');
  const [showAllThreads, setShowAllThreads] = useState(false);
  const [showProjectThreads, setShowProjectThreads] = useState(true);
  const [showScrollBottom, setShowScrollBottom] = useState(false);

  const [recentLogs, setRecentLogs] = useState<LogEntry[]>([]);
  const [pendingLogs, setPendingLogs] = useState<Record<string, LogEntry>>({});
  const [runningByThread, setRunningByThread] = useState<Record<string, string>>({});
  const [threadBuffers, setThreadBuffers] = useState<Record<string, ThreadBuffers>>({});
  const [activeThreadID, setActiveThreadID] = useState('');
  const [activeSessionID, setActiveSessionID] = useState('');

  const outputRef = useRef<HTMLDivElement>(null);
  const activeThreadIDRef = useRef('');
  const runningByThreadRef = useRef<Record<string, string>>({});
  const threadBuffersRef = useRef<Record<string, ThreadBuffers>>({});
  const flushTimerRef = useRef<number | null>(null);
  const stickToBottomRef = useRef(true);

  const activeThreadRunning = !!(activeThreadID && runningByThread[activeThreadID]);
  const isRunning = Object.keys(runningByThread).length > 0;
  const activeRunID = activeThreadID ? runningByThread[activeThreadID] || '' : '';

  const log = useCallback((level: string, msg: string) => {
    const line = `[${level}] ${msg}`;
    try { LogPrint(line); } catch {}
    try { WriteAppLog(line); } catch {}
  }, []);

  useEffect(() => {
    activeThreadIDRef.current = activeThreadID;
  }, [activeThreadID]);

  useEffect(() => {
    runningByThreadRef.current = runningByThread;
  }, [runningByThread]);

  useEffect(() => {
    threadBuffersRef.current = threadBuffers;
  }, [threadBuffers]);

  useEffect(() => () => {
    if (flushTimerRef.current !== null) {
      window.clearTimeout(flushTimerRef.current);
    }
  }, []);

  const blankThreadBuffers = useCallback((): ThreadBuffers => ({
    outputLines: [],
    rawOutput: [],
    error: '',
    sessionID: '',
  }), []);

  const flushThreadBuffers = useCallback(() => {
    flushTimerRef.current = null;
    setThreadBuffers(threadBuffersRef.current);
    const activeThreadID = activeThreadIDRef.current;
    if (!activeThreadID) return;
    const activeBuffers = threadBuffersRef.current[activeThreadID];
    if (!activeBuffers) return;
    setOutputLines(activeBuffers.outputLines);
    setRawOutput(activeBuffers.rawOutput);
    setError(activeBuffers.error);
    setActiveSessionID(activeBuffers.sessionID);
  }, []);

  const scheduleThreadBufferFlush = useCallback(() => {
    if (flushTimerRef.current !== null) return;
    flushTimerRef.current = window.setTimeout(flushThreadBuffers, 60);
  }, [flushThreadBuffers]);

  const writeThreadBuffers = useCallback((threadID: string, updater: (current: ThreadBuffers) => ThreadBuffers) => {
    if (!threadID) return;
    const current = threadBuffersRef.current[threadID] || blankThreadBuffers();
    const next = updater(current);
    threadBuffersRef.current = { ...threadBuffersRef.current, [threadID]: next };
    scheduleThreadBufferFlush();
  }, [blankThreadBuffers, scheduleThreadBufferFlush]);

  const switchToThread = useCallback((threadID: string) => {
    stickToBottomRef.current = true;
    setShowScrollBottom(false);
    activeThreadIDRef.current = threadID;
    setActiveThreadID(threadID);
    const buffers = threadBuffersRef.current[threadID] || blankThreadBuffers();
    setOutputLines(buffers.outputLines);
    setRawOutput(buffers.rawOutput);
    setError(buffers.error);
    setActiveSessionID(buffers.sessionID);
  }, [blankThreadBuffers]);

  const isNearBottom = useCallback((element: HTMLDivElement) => (
    element.scrollHeight - element.scrollTop - element.clientHeight <= SCROLL_BOTTOM_THRESHOLD
  ), []);

  const handleConversationScroll = useCallback(() => {
    const element = outputRef.current;
    if (!element) return;
    const nearBottom = isNearBottom(element);
    stickToBottomRef.current = nearBottom;
    setShowScrollBottom(!nearBottom);
  }, [isNearBottom]);

  const scrollToBottom = useCallback((behavior: ScrollBehavior = 'smooth') => {
    const element = outputRef.current;
    if (!element) return;
    stickToBottomRef.current = true;
    setShowScrollBottom(false);
    window.requestAnimationFrame(() => {
      element.scrollTo({ top: element.scrollHeight, behavior });
    });
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
        const threadID = event.thread_id || activeThreadIDRef.current;
        if (!threadID) return;
        const shouldAppendOutput = event.type === 'display' || event.type === 'status' || event.type === 'stderr' ||
          event.type === 'thinking-start' || event.type === 'thinking-delta' || event.type === 'thinking-end' ||
          event.type === 'tool-start' || event.type === 'tool-end';

        const exitCode = toFiniteNumber(event.meta?.exit_code);
        if (event.type === 'error') {
          log('ERROR', 'run-event error: ' + event.text);
        }
        if (event.type === 'done') {
          log('INFO', 'run-event done');
        }

        writeThreadBuffers(threadID, (current) => {
          let next = current;
          if (event.raw) {
            const rawLine = event.type === 'stderr' ? '[STDERR] ' + event.raw : event.raw;
            next = { ...next, rawOutput: appendRawLine(next.rawOutput, rawLine || '') };
          }
          if (shouldAppendOutput) {
            next = { ...next, outputLines: appendOutputLine(next.outputLines, event) };
          }
          if (event.type === 'done') {
            next = { ...next, outputLines: mergeDoneStats(next.outputLines, event) };
            if (Number.isFinite(exitCode) && exitCode !== 0) {
              next = { ...next, error: `运行退出码: ${exitCode}` };
            }
          }
          if (event.type === 'error') {
            next = { ...next, error: event.text };
          }
          if (event.claude_session_id) {
            next = { ...next, sessionID: event.claude_session_id || next.sessionID };
          }
          return next;
        });

        if (event.type === 'done' || event.type === 'error') {
          setRunningByThread((prev) => {
            const next = { ...prev };
            delete next[threadID];
            runningByThreadRef.current = next;
            return next;
          });
          const cleanupPending = () => {
            setPendingLogs((prev) => {
              const next = { ...prev };
              delete next[threadID];
              return next;
            });
          };
          if (event.type === 'done') {
            loadRecentLogs().finally(cleanupPending);
          } else {
            cleanupPending();
          }
        }
      } catch (err: any) {
        log('ERROR', 'run-event handler error: ' + String(err));
      }
    });

    return () => {
      EventsOff('run-event');
    };
  }, [loadRecentLogs, log, writeThreadBuffers]);

  useEffect(() => {
    if (outputRef.current) {
      if (stickToBottomRef.current) {
        scrollToBottom('auto');
      } else {
        setShowScrollBottom(true);
      }
    }
  }, [outputLines, rawOutput, scrollToBottom]);

  const truncate = (value: string, size: number) => (
    value && value.length > size ? value.slice(0, size - 1) + '…' : value
  );

  const effectiveModel = customModel.trim() || model;
  const selectedPermissionLabel = PERMISSION_MODES.find((item) => item.value === permissionMode)?.label || permissionMode;
  const projectName = projectPath ? projectPath.split(/[\\/]/).filter(Boolean).pop() || projectPath : 'Claude Tools';
  const pendingThreadIDs = new Set(Object.keys(pendingLogs));
  const mergedLogs = [
    ...Object.values(pendingLogs),
    ...recentLogs.filter((log) => !pendingThreadIDs.has(log.thread_id || log.id)),
  ].sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime());
  const activeLog = activeThreadID
    ? mergedLogs.find((log) => (log.thread_id || log.id) === activeThreadID)
    : null;
  const visibleLogs = projectPath
    ? mergedLogs.filter((log) => !log.project_path || log.project_path === projectPath)
    : mergedLogs;
  const outputSegments = useMemo(() => buildOutputSegments(outputLines), [outputLines]);
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
        switchToThread(createLocalID('new'));
        setPrompt('');
      }
    } catch (err: any) {
      const msg = '选择目录失败: ' + err;
      log('ERROR', msg);
      setError(msg);
    }
  };

  const handleClear = () => {
    if (activeThreadID) {
      writeThreadBuffers(activeThreadID, () => blankThreadBuffers());
    } else {
      setOutputLines([]);
      setRawOutput([]);
      setError('');
      setActiveSessionID('');
    }
  };

  const handleNewTask = () => {
    setPrompt('');
    switchToThread(createLocalID('new'));
  };

  const handleToggleProjectThreads = () => {
    setShowProjectThreads((current) => !current);
  };

  const handleProjectThread = async () => {
    if (!projectPath) {
      await handleSelectDir();
      return;
    }
    setPrompt('');
    setShowProjectThreads(true);
    switchToThread(createLocalID('new'));
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
      setPrompt('');
      switchToThread(createLocalID('new'));
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
      const nextBuffers = { ...threadBuffersRef.current };
      delete nextBuffers[threadID];
      threadBuffersRef.current = nextBuffers;
      setThreadBuffers(nextBuffers);
      setPendingLogs((prev) => {
        const next = { ...prev };
        delete next[threadID];
        return next;
      });
      if ((activeThreadID || activeSessionID) && activeThreadID === threadID) {
        activeThreadIDRef.current = '';
        setActiveThreadID('');
        setActiveSessionID('');
        setOutputLines([]);
        setRawOutput([]);
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
    const startedAt = new Date().toISOString();
    const existingBuffers = threadBuffersRef.current[threadID] || (
      activeThreadID === threadID
        ? { outputLines, rawOutput, error: '', sessionID: activeSessionID }
        : blankThreadBuffers()
    );
    const nextBuffers: ThreadBuffers = {
      ...existingBuffers,
      error: '',
      outputLines: [
        ...existingBuffers.outputLines,
        { type: 'user', text: trimmedPrompt, run_id: runID, thread_id: threadID },
      ],
    };

    stickToBottomRef.current = true;
    setShowScrollBottom(false);
    activeThreadIDRef.current = threadID;
    threadBuffersRef.current = { ...threadBuffersRef.current, [threadID]: nextBuffers };
    setThreadBuffers(threadBuffersRef.current);
    setActiveThreadID(threadID);
    setActiveSessionID(nextBuffers.sessionID);
    setOutputLines(nextBuffers.outputLines);
    setRawOutput(nextBuffers.rawOutput);
    setError('');
    setRunningByThread((prev) => {
      const next = { ...prev, [threadID]: runID };
      runningByThreadRef.current = next;
      return next;
    });
    setPendingLogs((prev) => ({
      ...prev,
      [threadID]: {
        id: runID,
        thread_id: threadID,
        claude_session_id: nextBuffers.sessionID,
        created_at: startedAt,
        project_path: projectPath,
        model: effectiveModel,
        permission_mode: mode,
        prompt: trimmedPrompt,
        display_output: '',
        raw_output: '',
        exit_code: 0,
        duration_ms: 0,
        input_tokens: 0,
        output_tokens: 0,
      } as LogEntry,
    }));

    log('INFO', `doStartRun: thread=${threadID} run=${runID} project=${projectPath} model=${effectiveModel} mode=${mode}`);

    setPrompt('');

    try {
      await StartRun({
        run_id: runID,
        project_path: projectPath,
        thread_id: threadID,
        claude_session_id: nextBuffers.sessionID,
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
      writeThreadBuffers(threadID, (current) => ({
        ...current,
        error: msg,
      }));
      setRunningByThread((prev) => {
        const next = { ...prev };
        delete next[threadID];
        runningByThreadRef.current = next;
        return next;
      });
      setPendingLogs((prev) => {
        const next = { ...prev };
        delete next[threadID];
        return next;
      });
    }
  };

  const handleStop = async () => {
    try {
      if (!activeRunID) {
        setError('当前线程没有正在运行的任务');
        return;
      }
      await StopRun(activeRunID);
      setRunningByThread((prev) => {
        const next = { ...prev };
        if (activeThreadID) delete next[activeThreadID];
        runningByThreadRef.current = next;
        return next;
      });
    } catch (err: any) {
      setError('停止失败: ' + err);
    }
  };

  const handleViewLog = async (log: LogEntry) => {
    const threadID = log.thread_id || log.id;
    const liveBuffers = threadBuffersRef.current[threadID];
    if (liveBuffers && (liveBuffers.outputLines.length > 0 || runningByThreadRef.current[threadID])) {
      stickToBottomRef.current = true;
      setShowScrollBottom(false);
      activeThreadIDRef.current = threadID;
      setActiveThreadID(threadID);
      setActiveSessionID(liveBuffers.sessionID || log.claude_session_id || '');
      setProjectPath(log.project_path || projectPath);
      setOutputLines(liveBuffers.outputLines);
      setRawOutput(liveBuffers.rawOutput);
      setError(liveBuffers.error);
      return;
    }

    const logs = await GetThreadLogs(threadID);
    const threadLogs = logs && logs.length ? logs : [log];
    const lastLog = threadLogs[threadLogs.length - 1];
    const nextRawOutput = threadLogs.flatMap((entry) => [
      `--- run ${entry.id.slice(0, 8)} / ${entry.created_at} ---`,
      ...(entry.raw_output ? entry.raw_output.split('\n') : []),
    ]);
    const nextOutputLines = threadLogs.flatMap((entry) => [
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
    ]);
    const nextError = lastLog.exit_code === 0 ? '' : `历史运行退出码: ${lastLog.exit_code}`;
    const nextBuffers: ThreadBuffers = {
      outputLines: nextOutputLines,
      rawOutput: nextRawOutput,
      error: nextError,
      sessionID: lastLog.claude_session_id || '',
    };

    stickToBottomRef.current = true;
    setShowScrollBottom(false);
    activeThreadIDRef.current = threadID;
    threadBuffersRef.current = { ...threadBuffersRef.current, [threadID]: nextBuffers };
    setThreadBuffers(threadBuffersRef.current);
    setActiveThreadID(threadID);
    setActiveSessionID(nextBuffers.sessionID);
    setProjectPath(lastLog.project_path || log.project_path);
    setRawOutput(nextBuffers.rawOutput);
    setError(nextBuffers.error);
    setOutputLines(nextBuffers.outputLines);
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
          <button onClick={handleNewTask}><span>□</span>新对话</button>
          <button><span>⌕</span>搜索</button>
          <button><span>⌘</span>插件</button>
          <button><span>◷</span>自动化</button>
        </nav>

        <section className="sidebar-group project-group">
          <div className="group-heading">
            <span>项目</span>
            <div className="group-actions">
              <button onClick={handleSelectDir} title="选择项目目录">↗</button>
              <button onClick={handleClear} disabled={activeThreadRunning} title="清空输出">≡</button>
            </div>
          </div>

          <div className="project-row-wrap">
            <button className="project-row" onClick={handleToggleProjectThreads} title={showProjectThreads ? '收起线程' : '展开线程'}>
              <span className="project-toggle">{showProjectThreads ? '▾' : '▸'}</span>
              <span className="folder-icon"><FolderIcon /></span>
              <span className="project-name">{projectName}</span>
            </button>
            <button className="project-new-thread" onClick={handleProjectThread} title={projectPath ? '开启新线程' : '选择项目目录'}>+</button>
          </div>
        </section>

        {showProjectThreads && <section className="sidebar-group history-group">
          <div className="run-list">
            {visibleLogs.length === 0 ? (
              <div className="empty-list">暂无聊天</div>
            ) : (
              (showAllThreads ? visibleLogs : visibleLogs.slice(0, 5)).map((log) => {
                const threadID = log.thread_id || log.id;
                const threadRunning = !!runningByThread[threadID];
                return (
                  <button
                    key={threadID}
                    className={`history-row ${threadID === activeThreadID ? 'active' : ''}`}
                    onClick={() => handleViewLog(log)}
                  >
                    <span>{truncate(log.prompt || '(empty prompt)', 28)}</span>
                    <time>{threadRunning ? '运行中' : formatAge(log.created_at)}</time>
                    {!threadRunning && <span className="delete-thread" onClick={(e) => handleDeleteThread(log, e)} title="删除线程">×</span>}
                  </button>
                );
              })
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
            {activeThreadRunning ? (
              <button className="pill-button danger" onClick={handleStop}>停止</button>
            ) : null}
          </div>
        </header>

        <div className="conversation-scroll" ref={outputRef} onScroll={handleConversationScroll}>
          <div className="conversation-lane">
            {error && <div className="error-banner">{error}{logPath ? <><br/><small>日志路径: {logPath}</small></> : ''}</div>}

            {outputLines.length === 0 && !error ? (
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
                    if (segment.type === 'activity') {
                      return <ActivityGroup key={`activity-${index}`} segment={segment} />;
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

        {showScrollBottom && (
          <button className="scroll-bottom-button" onClick={() => scrollToBottom()} title="跳到底部">
            ↓
          </button>
        )}

        <section className="composer-dock">
          <div className="composer-card">
            <textarea
              value={prompt}
              onChange={(event) => setPrompt(event.target.value)}
              placeholder="要求后续变更"
              disabled={activeThreadRunning}
            />

            <div className="composer-bar">
              <div className="composer-left">
                <button className="round-button" onClick={handleSelectDir} title="选择项目">+</button>
                <select value={permissionMode} onChange={(event) => setPermissionMode(event.target.value)} disabled={activeThreadRunning}>
                  {PERMISSION_MODES.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}
                </select>
              </div>

              <div className="composer-right">
                <select value={model} onChange={(event) => { setModel(event.target.value); setCustomModel(''); }} disabled={activeThreadRunning}>
                  {MODELS.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}
                  <option value="__custom__">Custom model</option>
                </select>
                <button className="send-button" onClick={() => doStartRun(permissionMode)} disabled={activeThreadRunning} title="提交">↑</button>
              </div>
            </div>
          </div>

          <div className="workspace-status">
            <span>本地模式</span>
            <span>master</span>
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
                disabled={activeThreadRunning}
              />
            </label>

            <label className="field">
              <span>Base URL</span>
              <input
                type="text"
                value={baseUrl}
                onChange={(event) => saveBaseUrl(event.target.value)}
                disabled={activeThreadRunning}
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
                  disabled={activeThreadRunning}
                />
              </label>
            )}

            <label className="field">
              <span>Language</span>
              <input
                type="text"
                value={language}
                onChange={(event) => setLanguage(event.target.value)}
                disabled={activeThreadRunning}
              />
            </label>

            <div className="settings-meta">
              <div><span>Project</span><strong title={projectPath}>{projectName}</strong></div>
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
