import { useEffect, useRef, useState } from 'react';
import { killSession, sessionCanWrite, type HostedSession } from '../api';
import { markSessionKilled } from '../hooks/useSessions';

export function KillSessionDialog({ session, open, onClose, onKilled }: {
  session: HostedSession;
  open: boolean;
  onClose: () => void;
  onKilled?: () => void;
}) {
  const [killing, setKilling] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const nonceRef = useRef('');
  useEffect(() => { nonceRef.current = ''; }, [session.id]);
  if (!open) return null;

  const confirm = async () => {
    setKilling(true);
    setError(null);
    try {
      if (!nonceRef.current) nonceRef.current = typeof crypto.randomUUID === 'function'
        ? crypto.randomUUID()
        : `v2-kill-${Date.now()}-${Math.random().toString(36).slice(2)}`;
      await killSession(session, nonceRef.current);
      markSessionKilled(session.sourceHost.id, session.id);
      onKilled?.();
      onClose();
    } catch (nextError) {
      setError(nextError instanceof Error ? nextError.message : String(nextError));
    } finally {
      setKilling(false);
    }
  };

  return (
    <div className="danger-dialog-backdrop" data-qa="kill-dialog" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget && !killing) onClose(); }}>
      <section className="danger-dialog" role="dialog" aria-modal="true" aria-labelledby={`kill-title-${session.id}`}>
        <div className="danger-dialog-mark" aria-hidden="true">!</div>
        <h2 id={`kill-title-${session.id}`}>彻底关闭 CLI？</h2>
        <dl>
          <dt>会话</dt><dd>{session.title}</dd>
          <dt>目录</dt><dd>{session.cwd}</dd>
          <dt>主机</dt><dd>{session.sourceHost.name}</dd>
        </dl>
        <p>将终止该 CLI 进程及其 tmux 窗格，不可恢复。</p>
        {error && <div className="danger-dialog-error" data-qa="kill-error">{error}</div>}
        <div className="danger-dialog-actions">
          <button type="button" onClick={onClose} disabled={killing}>取消</button>
          <button className="danger-confirm" data-qa="confirm-kill" type="button" onClick={confirm} disabled={killing || !sessionCanWrite(session)}>{killing ? '正在关闭…' : '彻底关闭'}</button>
        </div>
      </section>
    </div>
  );
}
