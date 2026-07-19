import { useEffect, useMemo, useRef, useState } from 'react';
import { getV2EntryScreen, sendV2EntryKey } from '../v2/liveData';
import type { V2EntryScreen, V2TerminalKey } from '../v2/types';

const SCREEN_REFRESH_MS = 1_000;
const QUICK_KEYS: Array<{ key: V2TerminalKey; label: string }> = [
  ...Array.from({ length: 6 }, (_, value) => ({ key: String(value) as V2TerminalKey, label: String(value) })),
  { key: 'Up', label: '↑' },
  { key: 'Down', label: '↓' },
  { key: 'Left', label: '←' },
  { key: 'Right', label: '→' },
  { key: 'Enter', label: 'Enter' },
  { key: 'Escape', label: 'Esc' },
  { key: 'Tab', label: 'Tab' },
  { key: 'Ctrl+C', label: 'Ctrl+C' },
];

interface AnsiStyle {
  color?: string;
  backgroundColor?: string;
  fontWeight?: number;
  opacity?: number;
  textDecoration?: string;
  inverse?: boolean;
}

interface AnsiSegment {
  text: string;
  style: AnsiStyle;
}

const ANSI_COLORS = ['#1b1d1e', '#ef5350', '#66bb6a', '#fbc02d', '#42a5f5', '#ab47bc', '#26c6da', '#e0e0e0'];
const ANSI_BRIGHT_COLORS = ['#616161', '#ff8a80', '#b9f6ca', '#ffe57f', '#82b1ff', '#ea80fc', '#84ffff', '#ffffff'];
const THEMED_HIGHLIGHT = { color: '#f3eadf', backgroundColor: '#3a3733' };

function ansi256(index: number): string {
  if (index < 8) return ANSI_COLORS[index];
  if (index < 16) return ANSI_BRIGHT_COLORS[index - 8];
  if (index < 232) {
    const value = index - 16;
    const levels = [0, 95, 135, 175, 215, 255];
    return `rgb(${levels[Math.floor(value / 36)]}, ${levels[Math.floor(value / 6) % 6]}, ${levels[value % 6]})`;
  }
  const gray = 8 + (index - 232) * 10;
  return `rgb(${gray}, ${gray}, ${gray})`;
}

function isLightNeutral(color: string | undefined): boolean {
  if (!color) return false;
  const hex = color.match(/^#([0-9a-f]{6})$/i)?.[1];
  const rgb = hex
    ? [0, 2, 4].map((offset) => Number.parseInt(hex.slice(offset, offset + 2), 16))
    : color.match(/^rgb\((\d+),\s*(\d+),\s*(\d+)\)$/)?.slice(1).map(Number);
  return Boolean(rgb && Math.min(...rgb) >= 180 && Math.max(...rgb) - Math.min(...rgb) <= 24);
}

function renderedStyle(style: AnsiStyle): AnsiStyle {
  const { inverse, ...plain } = style;
  const rendered = inverse ? {
    ...plain,
    color: plain.backgroundColor ?? '#f3eadf',
    backgroundColor: plain.color ?? '#344a3b',
  } : plain;
  return isLightNeutral(rendered.backgroundColor) ? { ...rendered, ...THEMED_HIGHLIGHT } : rendered;
}

function applySgr(style: AnsiStyle, parameters: string): AnsiStyle {
  const codes = (parameters || '0').split(';').map((value) => Number(value || 0));
  let next = { ...style };
  for (let index = 0; index < codes.length; index += 1) {
    const code = codes[index];
    if (code === 0) next = {};
    else if (code === 1) next.fontWeight = 700;
    else if (code === 2) next.opacity = 0.66;
    else if (code === 4) next.textDecoration = 'underline';
    else if (code === 7) next.inverse = true;
    else if (code === 22) { delete next.fontWeight; delete next.opacity; }
    else if (code === 24) delete next.textDecoration;
    else if (code === 27) delete next.inverse;
    else if (code === 39) delete next.color;
    else if (code === 49) delete next.backgroundColor;
    else if (code >= 30 && code <= 37) next.color = ANSI_COLORS[code - 30];
    else if (code >= 40 && code <= 47) next.backgroundColor = ANSI_COLORS[code - 40];
    else if (code >= 90 && code <= 97) next.color = ANSI_BRIGHT_COLORS[code - 90];
    else if (code >= 100 && code <= 107) next.backgroundColor = ANSI_BRIGHT_COLORS[code - 100];
    else if ((code === 38 || code === 48) && codes[index + 1] === 5 && Number.isInteger(codes[index + 2])) {
      const color = ansi256(Math.max(0, Math.min(255, codes[index + 2])));
      if (code === 38) next.color = color;
      else next.backgroundColor = color;
      index += 2;
    } else if ((code === 38 || code === 48) && codes[index + 1] === 2 && codes.slice(index + 2, index + 5).every(Number.isFinite)) {
      const color = `rgb(${codes[index + 2]}, ${codes[index + 3]}, ${codes[index + 4]})`;
      if (code === 38) next.color = color;
      else next.backgroundColor = color;
      index += 4;
    }
  }
  return next;
}

function ansiSegments(content: string): AnsiSegment[] {
  const segments: AnsiSegment[] = [];
  const pattern = new RegExp(`${String.fromCharCode(27)}\\[([0-?]*)([ -/]*)([@-~])`, 'g');
  let style: AnsiStyle = {};
  let cursor = 0;
  for (const match of content.matchAll(pattern)) {
    const index = match.index ?? 0;
    if (index > cursor) segments.push({ text: content.slice(cursor, index), style: renderedStyle(style) });
    if (match[3] === 'm') style = applySgr(style, match[1]);
    cursor = index + match[0].length;
  }
  if (cursor < content.length) segments.push({ text: content.slice(cursor), style: renderedStyle(style) });
  return segments;
}

function nonce(): string {
  return typeof crypto.randomUUID === 'function'
    ? crypto.randomUUID()
    : `screen-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

export function TerminalScreen({ hostId, entryId, enabled }: { hostId: string; entryId: string; enabled: boolean }) {
  const [screen, setScreen] = useState<V2EntryScreen | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [unsupported, setUnsupported] = useState(false);
  const [keyStatus, setKeyStatus] = useState<string | null>(null);
  const [keySending, setKeySending] = useState(false);
  const etagRef = useRef<string | undefined>(undefined);
  const segments = useMemo(() => ansiSegments(screen?.content ?? ''), [screen?.content]);

  useEffect(() => {
    const abort = new AbortController();
    let active = true;
    let timer: number | undefined;
    const schedule = () => {
      if (active && !document.hidden) timer = window.setTimeout(refresh, SCREEN_REFRESH_MS);
    };
    const refresh = async () => {
      let retry = true;
      try {
        const next = await getV2EntryScreen(hostId, entryId, etagRef.current, abort.signal);
        if (!active) return;
        etagRef.current = next.etag ?? etagRef.current;
        if (next.screen) setScreen(next.screen);
        setError(null);
      } catch (nextError) {
        if (!active || abort.signal.aborted) return;
        if (typeof nextError === 'object' && nextError !== null && 'status' in nextError && (nextError.status === 404 || nextError.status === 501)) {
          retry = false;
          setUnsupported(true);
          setError(null);
        } else {
          setError(nextError instanceof Error ? nextError.message : String(nextError));
        }
      } finally {
        if (retry) schedule();
      }
    };
    const onVisibility = () => {
      if (timer !== undefined) window.clearTimeout(timer);
      timer = undefined;
      if (!document.hidden) void refresh();
    };
    document.addEventListener('visibilitychange', onVisibility);
    if (!document.hidden) void refresh();
    return () => {
      active = false;
      abort.abort();
      if (timer !== undefined) window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [hostId, entryId]);

  const press = async (key: V2TerminalKey) => {
    if (!enabled || keySending) return;
    setKeySending(true);
    setKeyStatus(null);
    try {
      await sendV2EntryKey(hostId, entryId, key, nonce());
      setKeyStatus(`已发送 ${key === 'Escape' ? 'Esc' : key}`);
    } catch (nextError) {
      setKeyStatus(nextError instanceof Error ? nextError.message : String(nextError));
    } finally {
      setKeySending(false);
    }
  };

  return (
    <section className="terminal-screen-shell" data-qa="terminal-screen">
      <div className="terminal-screen-status" aria-live="polite">
        <span>终端画面 · 只作交互兜底</span>
        <small>{unsupported ? '服务端版本待升级' : error ? '暂不可用，正在重试…' : screen ? `${screen.cols}×${screen.rows}` : '正在连接…'}</small>
      </div>
      {unsupported ? <div className="terminal-screen-unavailable" data-qa="terminal-screen-unavailable">服务端版本待升级</div> : <div className="terminal-screen-scroller" data-qa="terminal-screen-scroller">
        <pre
          className="terminal-screen-content"
          data-cols={screen?.cols ?? ''}
          data-rows={screen?.rows ?? ''}
          data-cursor-x={screen?.cursorX ?? ''}
          data-cursor-y={screen?.cursorY ?? ''}
          data-hash={screen?.hash ?? ''}
          style={screen ? { minWidth: `${screen.cols}ch`, minHeight: `${screen.rows * 1.35}em` } : undefined}
        >
          {segments.map((segment, index) => <span style={segment.style} key={index}>{segment.text}</span>)}
          {screen && (
            <span
              className="terminal-screen-cursor"
              data-qa="terminal-screen-cursor"
              aria-hidden="true"
              style={{ left: `calc(10px + ${screen.cursorX}ch)`, top: `calc(10px + ${screen.cursorY * 1.35}em)` }}
            />
          )}
        </pre>
      </div>}
      <div className="terminal-key-panel" data-qa="terminal-key-panel" aria-label="终端快捷键">
        {QUICK_KEYS.map((item) => (
          <button
            type="button"
            data-terminal-key={item.key}
            disabled={!enabled || unsupported || keySending}
            onClick={() => void press(item.key)}
            key={item.key}
          >{item.label}</button>
        ))}
      </div>
      <div className="terminal-key-status" data-qa="terminal-key-status" aria-live="polite">
        {unsupported ? '服务端升级后即可使用终端画面与快捷键' : !enabled ? '终端写入尚未就绪' : keyStatus ?? '快捷键会直接发送到当前终端入口'}
      </div>
    </section>
  );
}
