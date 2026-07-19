import { Outlet } from 'react-router-dom';
import type { ReactNode } from 'react';
import { isMockMode } from '../api';

export function ConsoleLayout({ children }: { children?: ReactNode }) {
  return (
    <main className="app-shell">
      <header className="wa-header console-header">
        <div>
          <div className="console-title">AI 会话</div>
          <div className="console-subtitle">Claude 与 Codex 集中观察台</div>
        </div>
        <span className={`data-mode ${isMockMode() ? 'mock' : 'live'}`}>
          {isMockMode() ? 'MOCK' : 'LIVE'}
        </span>
      </header>
      {children ?? <Outlet />}
    </main>
  );
}
