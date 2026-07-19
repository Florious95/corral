import { Component, Fragment, type ErrorInfo, type ReactNode } from 'react';

interface State {
  error: Error | null;
  resetKey: number;
}

export class AppErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null, resetKey: 0 };

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('[ui] 已拦截渲染异常', error, info.componentStack);
  }

  render() {
    if (!this.state.error) return <Fragment key={this.state.resetKey}>{this.props.children}</Fragment>;
    return (
      <main className="app-shell global-error-shell" data-qa="global-error-boundary">
        <header className="wa-header"><span className="wa-header-title">AI 会话</span></header>
        <div className="global-error-bar" role="alert">
          <strong>页面操作失败</strong>
          <span>{this.state.error.message || '发生未知错误'}</span>
          <div className="global-error-actions">
            <button type="button" onClick={() => this.setState((state) => ({ error: null, resetKey: state.resetKey + 1 }))}>重试</button>
            <button type="button" onClick={() => location.assign(`/${location.search}`)}>返回会话列表</button>
          </div>
        </div>
      </main>
    );
  }
}
