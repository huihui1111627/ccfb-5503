import { createContext, useContext } from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import AppRoutes from './routes';
import { useWorkspace, type WorkspaceApi } from './lib/useWorkspace';
import { ProgressBar } from './components/ProgressBar';
import { ErrorBoundary } from './components/ErrorBoundary';

const WorkspaceContext = createContext<WorkspaceApi | null>(null);

export function useWorkspaceContext(): WorkspaceApi {
  const ctx = useContext(WorkspaceContext);
  if (!ctx) throw new Error('WorkspaceContext 尚未初始化');
  return ctx;
}

const NAV = [
  { to: '/case', label: '案件', index: '01' },
  { to: '/analysis', label: '镜像分析', index: '02' },
  { to: '/recovery', label: '文件恢复', index: '03' },
  { to: '/timeline', label: '时间线', index: '04' },
  { to: '/report', label: '报告', index: '05' },
];

export default function App() {
  const api = useWorkspace();
  const c = api.state.caseRecord;
  const location = useLocation();

  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="brand">
          <span className="brand-mark">SD</span>
          <div>
            <strong>存储卡取证工作站</strong>
            <small>只读镜像 · 全程监管链 · 本地运算</small>
          </div>
        </div>
        <div className="case-pill">
          {c ? (
            <>
              <span className="pill-label">在办案件</span>
              <NavLink to="/case" className="link">
                {c.caseNumber} · {c.title}
              </NavLink>
            </>
          ) : (
            <NavLink to="/case" className="btn small">新建案件</NavLink>
          )}
        </div>
      </header>

      <nav className="sidebar">
        {NAV.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}
          >
            <span className="nav-index">{item.index}</span>
            {item.label}
          </NavLink>
        ))}
        <div className="sidebar-foot">
          <span className={`dot ${api.state.busy ? 'busy' : ''}`} />
          <span className="ellipsis">{api.state.busy ?? '空闲'}</span>
        </div>
      </nav>

      <main className="content">
        <WorkspaceContext.Provider value={api}>
          {api.state.error && (
            <div className="alert error" role="alert">
              <strong>操作失败</strong>
              <span>{api.state.error}</span>
              <button className="link" onClick={api.clearError}>关闭</button>
            </div>
          )}
          {api.state.progress && <ProgressBar progress={api.state.progress} />}
          <ErrorBoundary key={location.pathname}>
            <AppRoutes />
          </ErrorBoundary>
        </WorkspaceContext.Provider>
      </main>
    </div>
  );
}
