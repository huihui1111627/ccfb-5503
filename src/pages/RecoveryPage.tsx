import { useMemo, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { useWorkspaceContext } from '../App';
import { formatBytes } from '../core/report/pdf';
import type { FileEntry } from '../core/types';
import type { UiRecovery } from '../lib/useWorkspace';
import { EmptyState } from './AnalysisPage';

type Filter = 'all' | 'deleted' | 'files' | 'dirs';
type SortKey = 'path' | 'size' | 'created' | 'status';

export default function RecoveryPage() {
  const api = useWorkspaceContext();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [filter, setFilter] = useState<Filter>('all');
  const [query, setQuery] = useState('');
  const [sort, setSort] = useState<SortKey>('path');
  const [selected, setSelected] = useState<FileEntry | null>(null);
  const [carveOpen, setCarveOpen] = useState(params.get('carve') === '1');
  const [maxHits, setMaxHits] = useState(500);
  const [maxObjectMb, setMaxObjectMb] = useState(512);

  const entries = api.state.entries;
  const recoveries = api.state.recoveries;
  const carveHits = api.state.carveHits;
  const recoveryById = useMemo(() => new Map(recoveries.map((r) => [r.entryId, r])), [recoveries]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    let list = entries.filter((e) => {
      if (filter === 'deleted' && !e.deleted) return false;
      if (filter === 'files' && e.kind !== 'file') return false;
      if (filter === 'dirs' && e.kind !== 'directory') return false;
      if (q && !e.path.toLowerCase().includes(q)) return false;
      return true;
    });
    list = [...list].sort((a, b) => {
      switch (sort) {
        case 'size': return b.size - a.size;
        case 'created': return (a.times.created?.iso ?? '').localeCompare(b.times.created?.iso ?? '');
        case 'status': return (recoveryById.get(a.id)?.status ?? 'z').localeCompare(recoveryById.get(b.id)?.status ?? 'z');
        default: return a.path.localeCompare(b.path);
      }
    });
    return list;
  }, [entries, filter, query, sort, recoveryById]);

  if (!api.state.activeImage || !api.state.analysis) {
    return (
      <div className="page">
        <EmptyState
          title="请先在“镜像分析”页导入镜像并完成扫描"
          action={<button className="btn primary" onClick={() => navigate('/analysis')}>前往镜像分析</button>}
        />
      </div>
    );
  }

  if (entries.length === 0) {
    return (
      <div className="page">
        <EmptyState
          title="尚无扫描结果"
          action={<button className="btn primary" onClick={() => navigate('/analysis')}>返回扫描</button>}
        />
      </div>
    );
  }

  async function recoverSelected(entry: FileEntry) {
    await api.recover(entry);
  }

  async function recoverAllDeleted() {
    const deleted = entries.filter((e) => e.deleted && e.kind === 'file');
    for (const e of deleted) {
      // 顺序执行，避免多个大文件同时占满内存
      await api.recover(e);
    }
  }

  async function runCarve() {
    await api.carve({ maxHits, maxObjectSize: maxObjectMb * 1024 * 1024 });
  }

  const selectedRecovery = selected ? recoveryById.get(selected.id) : undefined;

  return (
    <div className="page">
      {api.state.restoredNotice && (
        <div className="notice">
          {api.state.restoredNotice}
          <label className="btn small" style={{ marginLeft: 12, cursor: 'pointer' }}>
            重新选择镜像…
            <input
              type="file"
              style={{ display: 'none' }}
              accept=".dd,.img,.raw,.bin,.001,application/octet-stream"
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) api.reattachImage(f);
              }}
            />
          </label>
        </div>
      )}
      <div className="page-head">
        <div>
          <h1>文件与恢复</h1>
          <p className="muted">
            共 {entries.length} 个条目（{api.state.stats?.deletedFiles ?? 0} 个已删除文件）。
            恢复结果遵循“不完整即标注”的原则。
          </p>
        </div>
        <div className="row">
          <button className="btn ghost" onClick={recoverAllDeleted}>
            恢复全部已删除文件
          </button>
          <button className="btn" onClick={() => setCarveOpen((v) => !v)}>
            {carveOpen ? '收起雕刻' : '雕刻未分配空间'}
          </button>
        </div>
      </div>

      <section className="card">
        <div className="toolbar">
          <div className="segmented">
            {([
              ['all', '全部'],
              ['deleted', '已删除'],
              ['files', '文件'],
              ['dirs', '目录'],
            ] as const).map(([key, label]) => (
              <button key={key} className={filter === key ? 'active' : ''} onClick={() => setFilter(key)}>
                {label}
              </button>
            ))}
          </div>
          <input
            className="search"
            placeholder="按路径或文件名筛选…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          <label className="field inline">
            <span>排序</span>
            <select value={sort} onChange={(e) => setSort(e.target.value as SortKey)}>
              <option value="path">路径</option>
              <option value="size">大小</option>
              <option value="created">创建时间</option>
              <option value="status">恢复状态</option>
            </select>
          </label>
        </div>

        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>状态</th><th>名称</th><th>路径</th><th>大小</th><th>簇链</th><th>创建时间</th><th>恢复</th>
              </tr>
            </thead>
            <tbody>
              {filtered.slice(0, 2000).map((e) => {
                const rec = recoveryById.get(e.id);
                return (
                  <tr key={e.id} className={`${e.deleted ? 'deleted' : ''} ${selected?.id === e.id ? 'selected' : ''}`}
                    onClick={() => setSelected(e)}>
                    <td>
                      {e.deleted
                        ? <span className="badge warn">已删除</span>
                        : <span className="badge ok">现存</span>}
                    </td>
                    <td className="ellipsis" title={e.name}>
                      {e.kind === 'directory' ? '📁 ' : ''}{e.name}
                      {e.isMetadata && <span className="badge tiny" title="系统元数据，未计入统计">系统</span>}
                      {e.nameConfidence !== 'exact' && (
                        <span className={`name-flag ${e.nameConfidence}`} title={e.nameNote ?? ''}>
                          {e.nameConfidence === 'high' ? '长名已恢复' : e.nameConfidence === 'medium' ? '名称待核' : '名称存疑'}
                        </span>
                      )}
                    </td>
                    <td className="mono ellipsis" title={e.path}>{e.path}</td>
                    <td className="mono">{e.kind === 'directory' ? '—' : formatBytes(e.size)}</td>
                    <td><ChainBadge entry={e} /></td>
                    <td className="mono">{e.times.created?.iso ?? '—'}</td>
                    <td onClick={(ev) => ev.stopPropagation()}>
                      {e.kind === 'file' && (
                        rec
                          ? <RecoveryBadge recovery={rec} />
                          : <button className="btn small" onClick={() => recoverSelected(e)}>恢复</button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        {filtered.length > 2000 && (
          <p className="muted">仅显示前 2000 条，请使用筛选缩小范围。</p>
        )}
      </section>

      {selected && (
        <section className="card detail">
          <div className="card-head">
            <h2>{selected.name}</h2>
            <button className="btn small ghost" onClick={() => setSelected(null)}>关闭</button>
          </div>
          <div className="grid-3">
            <Info label="路径" value={selected.path} />
            <Info label="类型" value={selected.kind === 'file' ? '文件' : '目录'} />
            <Info label="状态" value={selected.deleted ? '已删除' : '现存'} />
            <Info label="声明大小" value={`${selected.size.toLocaleString()} 字节`} />
            <Info label="起始簇" value={String(selected.firstCluster)} />
            <Info label="目录项偏移" value={selected.entryOffset.toLocaleString()} mono />
            <Info label="簇链状态" value={chainLabel(selected.chainQuality)} />
            <Info label="已分配簇" value={`${selected.allocatedClusters}`} />
            <Info label="覆盖簇" value={`${selected.overwrittenClusters}`} tone={selected.overwrittenClusters > 0 ? 'danger' : undefined} />
            <Info label="创建时间" value={selected.times.created?.iso ?? '—'} />
            <Info label="修改时间" value={selected.times.modified?.iso ?? '—'} />
            <Info label="访问时间" value={selected.times.accessed?.iso ?? '—'} />
          </div>
          {selected.runs.length > 0 && (
            <p className="muted">
              簇区间：{selected.runs.map((r) => `${r.start}+${r.count}`).join('，')}
              {selected.nameNote ? ` · ${selected.nameNote}` : ''}
            </p>
          )}
          {selected.warnings.length > 0 && (
            <ul className="warnings">
              {selected.warnings.map((w, i) => <li key={i}>{w}</li>)}
            </ul>
          )}
          {selectedRecovery && (
            <div className="recovery-result">
              <div className="card-head">
                <h3>恢复结果</h3>
                <StatusBadge status={selectedRecovery.status} />
              </div>
              <div className="grid-3">
                <Info label="恢复方式" value={methodLabel(selectedRecovery.method)} />
                <Info label="已恢复" value={`${selectedRecovery.recoveredSize.toLocaleString()} / ${selectedRecovery.requestedSize.toLocaleString()} 字节`} />
                <Info label="缺失字节" value={selectedRecovery.missingBytes.toLocaleString()} tone={selectedRecovery.missingBytes > 0 ? 'danger' : undefined} />
                <Info label="MD5" value={selectedRecovery.hash?.md5 ?? '—'} mono />
                <Info label="SHA-256" value={selectedRecovery.hash?.sha256 ?? '—'} mono />
              </div>
              {selectedRecovery.reasons.length > 0 && (
                <ul className="warnings">
                  {selectedRecovery.reasons.map((r, i) => <li key={i}>{r}</li>)}
                </ul>
              )}
              {selectedRecovery.blobUrl && (
                <a className="btn" href={selectedRecovery.blobUrl} download={safeName(selected.name)}>
                  下载恢复文件
                </a>
              )}
            </div>
          )}
          {!selectedRecovery && selected.kind === 'file' && (
            <button className="btn primary" onClick={() => recoverSelected(selected)}>恢复该文件</button>
          )}
        </section>
      )}

      {carveOpen && (
        <section className="card">
          <div className="card-head">
            <h2>未分配空间雕刻</h2>
            <span className="muted">按 MP4 / JPEG 文件签名扫描，跳过现存文件占用的簇</span>
          </div>
          <div className="form-row">
            <label className="field inline">
              <span>最多命中</span>
              <input type="number" min={1} max={5000} value={maxHits} onChange={(e) => setMaxHits(Number(e.target.value))} />
            </label>
            <label className="field inline">
              <span>单对象上限 (MiB)</span>
              <input type="number" min={1} max={4096} value={maxObjectMb} onChange={(e) => setMaxObjectMb(Number(e.target.value))} />
            </label>
            <button className="btn primary" onClick={runCarve} disabled={!!api.state.busy}>开始雕刻</button>
          </div>
          {carveHits.length === 0 ? (
            <p className="muted">尚未发现雕刻对象。</p>
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr><th>类型</th><th>偏移</th><th>大小</th><th>完整性</th><th>SHA-256</th><th>说明</th><th>下载</th></tr>
                </thead>
                <tbody>
                  {carveHits.map((h) => (
                    <tr key={h.id}>
                      <td><span className={`badge ${h.type === 'mp4' ? 'accent' : 'ok'}`}>{h.type.toUpperCase()}</span></td>
                      <td className="mono">{h.offset.toLocaleString()}</td>
                      <td className="mono">{formatBytes(h.size)}</td>
                      <td>{h.complete ? <span className="badge ok">完整</span> : <span className="badge warn">不完整</span>}</td>
                      <td className="mono ellipsis" title={h.hash.sha256}>{h.hash.sha256.slice(0, 20)}…</td>
                      <td className="ellipsis" title={h.reasons.join('；')}>{h.reasons.join('；') || '结构完整'}</td>
                      <td>
                        {h.blobUrl
                          ? <a className="link" href={h.blobUrl} download={`carved-${h.type}-${h.offset}.${h.type}`}>下载</a>
                          : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      )}
    </div>
  );
}

function ChainBadge({ entry }: { entry: FileEntry }) {
  const map: Record<FileEntry['chainQuality'], { label: string; tone: string }> = {
    valid: { label: '完整', tone: 'ok' },
    'contiguous-assumed': { label: '连续假设', tone: 'warn' },
    truncated: { label: '不完整', tone: 'warn' },
    empty: { label: '空', tone: '' },
    loop: { label: '成环', tone: 'danger' },
    overwritten: { label: '已覆盖', tone: 'danger' },
    error: { label: '错误', tone: 'danger' },
  };
  const v = map[entry.chainQuality];
  return <span className={`badge ${v.tone}`}>{v.label}</span>;
}

function chainLabel(q: FileEntry['chainQuality']): string {
  return {
    valid: '完整',
    'contiguous-assumed': 'FAT 链缺失，按连续簇恢复',
    truncated: '不完整（簇链短于文件大小）',
    empty: '无数据簇',
    loop: '簇链成环',
    overwritten: '数据簇已被重新分配',
    error: '解析错误',
  }[q];
}

function RecoveryBadge({ recovery }: { recovery: UiRecovery }) {
  return (
    <span className="recovery-inline">
      <StatusBadge status={recovery.status} />
      <span className="mono small-mono">{recovery.hash?.sha256.slice(0, 8)}</span>
    </span>
  );
}

function StatusBadge({ status }: { status: UiRecovery['status'] }) {
  const map: Record<UiRecovery['status'], { label: string; tone: string }> = {
    complete: { label: '完整恢复', tone: 'ok' },
    partial: { label: '部分恢复', tone: 'warn' },
    overwritten: { label: '已覆盖', tone: 'danger' },
    failed: { label: '恢复失败', tone: 'danger' },
  };
  const v = map[status];
  return <span className={`badge ${v.tone}`}>{v.label}</span>;
}

function methodLabel(m: UiRecovery['method']): string {
  return {
    'fat-chain': 'FAT 簇链',
    contiguous: '连续簇假设',
    'exfat-chain': 'exFAT FAT 链',
    'exfat-contiguous': 'exFAT 连续分配',
    none: '无',
  }[m];
}

function Info({ label, value, mono, tone }: { label: string; value: string; mono?: boolean; tone?: 'ok' | 'warn' | 'danger' | undefined }) {
  return (
    <div className="kv">
      <span>{label}</span>
      <strong className={`${mono ? 'mono' : ''} ${tone ?? ''}`}>{value}</strong>
    </div>
  );
}

function safeName(name: string): string {
  return name.replace(/[\\/:*?"<>|]/g, '_');
}
