import { Component, ReactNode } from 'react';

interface State { hasError: boolean; message: string }

/** 捕获渲染错误，避免整页白屏；取证工作台不应因单页异常丢失状态。 */
export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { hasError: false, message: '' };

  static getDerivedStateFromError(err: unknown): State {
    return { hasError: true, message: err instanceof Error ? err.message : String(err) };
  }

  render(): ReactNode {
    if (this.state.hasError) {
      return (
        <div className="page">
          <div className="empty">
            <strong>页面渲染出错</strong>
            <span className="muted mono">{this.state.message}</span>
            <button className="btn" onClick={() => this.setState({ hasError: false, message: '' })}>重试</button>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}
