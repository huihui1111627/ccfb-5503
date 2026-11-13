import { useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useWorkspaceContext } from '../App';
import { bucketTimeline } from '../core/timeline';
import { formatBytes } from '../core/report/pdf';
import { EmptyState } from './AnalysisPage';

type KindFilter = 'all' | 'created' | 'modified' | 'accessed';

export default function TimelinePage() {
  const api = useWorkspaceContext();
  const navigate = useNavigate();
  const [kind, setKind] = useState<KindFilter>('all');
  const [onlyDeleted, setOnlyDeleted] = useState(false);
  const [bucket, setBucket] = useState<'hour' | 'day'>('hour');
  const [limit, setLimit] = useState(300);

  const timeline = api.state.timeline;

  const filtered = useMemo(() => {
    if (!timeline) return [];
    return timeline.events.filter((e) => {
      if (kind !== 'all' && e.kind !== kind) return false;
      if (onlyDeleted && !e.deleted) return false;
      return true;
    });
  }, [timeline, kind, onlyDeleted]);

  const buckets = useMemo(() => bucketTimeline(filtered, bucket), [filtered, bucket]);

  if (!api.state.timeline) {
    return (
      <div className="page">
        <EmptyState
          title="尚未构建时间线"
          action={
            <div className="row">
              <button className="btn primary" onClick={() => api.buildTimeline()} disabled={!api.state.stats}>
                构建时间线
              </button>
            </div>
          }
        />
      </div>
    );
  }

  const maxBucket = Math.max(1, ...buckets.map((b) => b.count));

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>时间线</h1>
          <p className="muted">
            {timeline!.events.length} 个事件 · 最早 {timeline!.bounds.earliest ? fmt(timeline!.bounds.earliest) : '—'} ·
            最晚 {timeline!.bounds.latest ? fmt(timeline!.bounds.latest) : '—'}
            {timeline!.invalidTimestamps > 0 && ` · 已排除 ${timeline!.invalidTimestamps} 个无效时间戳`}
          </p>
        </div>
        <div className="row">
          <button className="btn ghost" onClick={() => api.buildTimeline()}>重新构建</button>
          <button className="btn" onClick={() => navigate('/report')}>生成报告 →</button>
        </div>
      </div>

      <section className="card">
        <div className="toolbar">
          <div className="segmented">
            {([
              ['all', '全部事件'],
              ['created', '创建'],
              ['modified', '修改'],
              ['accessed', '访问'],
            ] as const).map(([k, label]) => (
              <button key={k} className={kind === k ? 'active' : ''} onClick={() => setKind(k)}>{label}</button>
            ))}
          </div>
          <label className="check">
            <input type="checkbox" checked={onlyDeleted} onChange={(e) => setOnlyDeleted(e.target.checked)} />
            仅已删除
          </label>
          <div className="segmented">
            <button className={bucket === 'hour' ? 'active' : ''} onClick={() => setBucket('hour')}>按小时</button>
            <button className={bucket === 'day' ? 'active' : ''} onClick={() => setBucket('day')}>按天</button>
          </div>
        </div>

        <div className="histogram" role="img" aria-label="事件时间分布">
          {buckets.map((b) => (
            <div key={b.key} className="hist-col" title={`${b.key}：${b.count} 个事件（已删除 ${b.deleted}）`}>
              <div className="hist-bar" style={{ height: `${Math.max(2, (b.count / maxBucket) * 100)}%` }}>
                {b.deleted > 0 && <div className="hist-deleted" style={{ height: `${(b.deleted / b.count) * 100}%` }} />}
              </div>
              <span className="hist-label">{b.key.replace('T', ' ').slice(5)}</span>
            </div>
          ))}
        </div>
        <p className="muted legend">
          <span className="swatch" /> 全部事件　<span className="swatch deleted" /> 已删除文件事件
        </p>
      </section>

      <section className="card">
        <div className="card-head">
          <h2>事件列表</h2>
          <span className="muted">共 {filtered.length} 条</span>
        </div>
        <div className="table-scroll tall">
          <table className="table compact">
            <thead>
              <tr>
                <th className="col-time">时间</th>
                <th className="col-kind">类型</th>
                <th className="col-name">文件名</th>
                <th>路径</th>
                <th className="col-size">大小</th>
                <th className="col-tz">时区依据</th>
                <th className="col-note">备注</th>
              </tr>
            </thead>
            <tbody>
              {filtered.slice(0, limit).map((e) => (
                <tr key={e.id} className={e.deleted ? 'deleted' : ''}>
                  <td className="mono nowrap">
                    {e.dateOnly ? e.at.slice(0, 10) : fmt(e.at)}
                    {e.dateOnly && <span className="badge tiny" title="仅日期精度">仅日期</span>}
                  </td>
                  <td><span className={`badge ${e.kind === 'created' ? 'ok' : e.kind === 'modified' ? 'accent' : ''}`}>
                    {{ created: '创建', modified: '修改', accessed: '访问' }[e.kind]}
                  </span></td>
                  <td className="ellipsis" title={e.name}>{e.name}{e.deleted && <span className="badge warn tiny">已删</span>}</td>
                  <td className="mono ellipsis" title={e.path}>{e.path}</td>
                  <td className="mono">{formatBytes(e.size)}</td>
                  <td className="nowrap">{e.timezoneKnown ? '磁盘显式偏移' : '案件时区'}</td>
                  <td className="ellipsis col-note-cell" title={e.warnings.join('；')}>
                    {e.dateOnly ? '仅日期精度' : e.warnings.length ? e.warnings[0] : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {filtered.length > limit && (
          <button className="btn" onClick={() => setLimit((n) => n + 300)}>
            加载更多（剩余 {filtered.length - limit} 条）
          </button>
        )}
      </section>
    </div>
  );
}

function fmt(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const pad = (v: number) => String(v).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
