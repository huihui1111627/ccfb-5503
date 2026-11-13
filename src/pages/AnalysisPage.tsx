import { useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useWorkspaceContext } from '../App';
import { formatBytes, formatOffset } from '../core/report/pdf';
import type { VolumeInfo } from '../core/types';

export default function AnalysisPage() {
  const api = useWorkspaceContext();
  const navigate = useNavigate();
  const inputRef = useRef<HTMLInputElement>(null);
  const [includeDeleted, setIncludeDeleted] = useState(true);
  const [maxEntries, setMaxEntries] = useState(200_000);
  const [dragOver, setDragOver] = useState(false);
  const [verifyResult, setVerifyResult] = useState<{ ok: boolean } | null>(null);

  const c = api.state.caseRecord;
  const image = api.state.activeImage;
  const analysis = api.state.analysis;
  const activeVolume = analysis?.volumes[0];
  const stats = api.state.stats;

  async function onFile(file: File | undefined) {
    if (!file) return;
    await api.importImage(file);
  }

  async function onScan() {
    if (!activeVolume) return;
    await api.scan({ volume: activeVolume, includeDeleted, maxEntries });
  }

  async function onVerify() {
    const res = await api.verifyIntegrity();
    setVerifyResult({ ok: res.ok });
  }

  if (!c) {
    return (
      <div className="page">
        <EmptyState title="请先创建或打开案件" action={<button className="btn primary" onClick={() => navigate('/case')}>前往案件页</button>} />
      </div>
    );
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>镜像分析</h1>
          <p className="muted">导入 dd / img 镜像，自动识别 MBR、GPT 与 FAT32 / exFAT 卷；镜像不会被写入。</p>
        </div>
        <div className="row">
          <button className="btn ghost" onClick={() => inputRef.current?.click()}>选择镜像…</button>
          {image && <button className="btn" onClick={onVerify}>复核镜像哈希</button>}
        </div>
      </div>

      <input
        ref={inputRef}
        type="file"
        accept=".dd,.img,.raw,.bin,.001,application/octet-stream"
        hidden
        onChange={(e) => onFile(e.target.files?.[0])}
      />

      <section
        className={`dropzone ${dragOver ? 'over' : ''} ${image ? 'compact' : ''}`}
        onDragOver={(e) => { e.preventDefault(); setDragOver(true); }}
        onDragLeave={() => setDragOver(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDragOver(false);
          onFile(e.dataTransfer.files?.[0]);
        }}
        onClick={() => inputRef.current?.click()}
      >
        <strong>{image ? `当前镜像：${image.fileName}` : '拖入或点击选择 dd 镜像'}</strong>
        <span className="muted">
          {image
            ? `${formatBytes(image.size)} · ${image.partitions.length} 个分区`
            : '支持裸卷镜像与带 MBR/GPT 分区表的整盘镜像；文件不会被上传。'}
        </span>
      </section>

      {image && (
        <section className="card">
          <h2>镜像摘要</h2>
          <div className="grid-3">
            <Metric label="MD5" value={image.md5 ?? '—'} mono />
            <Metric label="SHA-256" value={image.sha256 ?? '—'} mono />
            <Metric
              label="完整性"
              value={
                verifyResult
                  ? verifyResult.ok ? '复核一致（只读）' : '复核不一致'
                  : image.verification === 'verified' ? '分析后复核一致' : '仅记录导入哈希'
              }
              tone={verifyResult ? (verifyResult.ok ? 'ok' : 'danger') : undefined}
            />
          </div>
          {analysis?.warnings.length ? (
            <ul className="warnings">
              {analysis.warnings.map((w, i) => <li key={i}>{w}</li>)}
            </ul>
          ) : null}
        </section>
      )}

      {analysis && (
        <section className="card">
          <div className="card-head">
            <h2>分区与卷</h2>
            <span className="muted">共 {analysis.partitions.length} 个分区，{analysis.volumes.length} 个可解析卷</span>
          </div>
          <table className="table">
            <thead>
              <tr>
                <th>#</th><th>方案</th><th>偏移</th><th>大小</th><th>类型</th><th>文件系统</th><th>卷标</th><th>簇大小</th><th>簇数</th><th>空闲簇</th>
              </tr>
            </thead>
            <tbody>
              {analysis.partitions.map((p) => (
                <tr key={`${p.scheme}-${p.index}`} className={p.volume?.id === activeVolume?.id ? 'selected' : ''}>
                  <td className="mono">{p.index}</td>
                  <td>{p.scheme.toUpperCase()}</td>
                  <td className="mono">{p.offset.toLocaleString()}</td>
                  <td className="mono">{formatBytes(p.size)}</td>
                  <td>{p.typeLabel}</td>
                  <td>{p.volume ? <FsBadge fs={p.volume.fs} /> : <span className="badge">未识别</span>}</td>
                  <td>{p.volume?.label || '—'}</td>
                  <td className="mono">{p.volume ? `${p.volume.clusterSize} B` : '—'}</td>
                  <td className="mono">{p.volume?.clusterCount.toLocaleString() ?? '—'}</td>
                  <td className="mono">{p.volume?.freeClusters?.toLocaleString() ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {analysis.volumes.length === 0 && (
            <p className="muted">未发现可解析的 FAT32/exFAT 卷。请确认镜像是否完整或包含分区表。</p>
          )}
        </section>
      )}

      {activeVolume && (
        <section className="card">
          <div className="card-head">
            <h2>卷详情</h2>
            <span className="badge">{activeVolume.fs.toUpperCase()}</span>
          </div>
          <div className="grid-3">
            <Metric label="卷标" value={activeVolume.label || '—'} />
            <Metric label="序列号" value={activeVolume.serial} mono />
            <Metric label="卷起始偏移" value={activeVolume.offset.toLocaleString()} mono />
            <Metric label="数据区偏移" value={activeVolume.dataOffset.toLocaleString()} mono />
            <Metric label="FAT 偏移" value={activeVolume.fatOffset.toLocaleString()} mono />
            <Metric label="FAT 大小" value={formatBytes(activeVolume.fatSize)} />
            <Metric label="每簇字节" value={`${activeVolume.clusterSize}`} />
            <Metric label="簇数" value={activeVolume.clusterCount.toLocaleString()} />
            <Metric label="空闲簇" value={activeVolume.freeClusters?.toLocaleString() ?? '未知'} />
            <Metric label="空闲数来源" value={sourceLabel(activeVolume.freeCountSource)} />
            <Metric label="备份引导区" value={activeVolume.backupBootValid === null ? '—' : activeVolume.backupBootValid ? '有效' : '无效'} />
            <Metric label="根目录簇" value={activeVolume.rootCluster === null ? '固定区' : String(activeVolume.rootCluster)} mono />
          </div>
          {activeVolume.warnings.length > 0 && (
            <ul className="warnings">
              {activeVolume.warnings.map((w, i) => <li key={i}>{w.message}</li>)}
            </ul>
          )}
        </section>
      )}

      {activeVolume && (
        <section className="card">
          <div className="card-head">
            <h2>目录扫描</h2>
            <span className="muted">只读解析目录项与簇链</span>
          </div>
          <div className="form-row">
            <label className="check">
              <input type="checkbox" checked={includeDeleted} onChange={(e) => setIncludeDeleted(e.target.checked)} />
              包含已删除条目
            </label>
            <label className="field inline">
              <span>条目上限</span>
              <input type="number" min={1000} step={1000} value={maxEntries}
                onChange={(e) => setMaxEntries(Number(e.target.value))} />
            </label>
            <button className="btn primary" onClick={onScan} disabled={!!api.state.busy}>开始扫描</button>
          </div>
          {stats && (
            <div className="grid-4 stats">
              <Metric label="现存文件" value={`${stats.files}`} />
              <Metric label="已删除文件" value={`${stats.deletedFiles}`} tone={stats.deletedFiles > 0 ? 'warn' : undefined} />
              <Metric label="目录" value={`${stats.directories}`} />
              <Metric label="恢复长文件名" value={`${stats.recoveredName}`} />
              <Metric label="现存数据量" value={formatBytes(stats.totalBytes)} />
              <Metric label="扫描耗时" value={`${(stats.durationMs / 1000).toFixed(2)} s`} />
              <Metric label="条目截断" value={stats.truncatedListing ? '是' : '否'} tone={stats.truncatedListing ? 'warn' : undefined} />
              <Metric label="解析警告" value={`${stats.warnings.length}`} />
              <Metric
                label="系统元数据"
                value={stats.metadataFiles ? `${stats.metadataFiles}（已排除）` : '无'}
              />
            </div>
          )}
          {stats && (
            <div className="row-actions">
              <button className="btn" onClick={() => navigate('/recovery')}>查看文件与恢复 →</button>
              <button className="btn ghost" onClick={() => navigate('/recovery?carve=1')}>未分配空间雕刻 →</button>
            </div>
          )}
        </section>
      )}
    </div>
  );
}

function FsBadge({ fs }: { fs: VolumeInfo['fs'] }) {
  return <span className={`badge ${fs === 'exfat' ? 'accent' : 'ok'}`}>{fs.toUpperCase()}</span>;
}

function sourceLabel(s: VolumeInfo['freeCountSource']): string {
  return { fsinfo: 'FSInfo', bitmap: '分配位图', 'fat-scan': 'FAT 扫描', unknown: '未知' }[s];
}

export function Metric({ label, value, mono, tone }: { label: string; value: string; mono?: boolean; tone?: 'ok' | 'warn' | 'danger' | undefined }) {
  return (
    <div className="metric">
      <span>{label}</span>
      <strong className={`${mono ? 'mono' : ''} ${tone ?? ''} ellipsis`} title={value}>{value}</strong>
    </div>
  );
}

export function EmptyState({ title, action }: { title: string; action?: React.ReactNode }) {
  return (
    <div className="empty">
      <strong>{title}</strong>
      {action}
    </div>
  );
}

export { formatOffset };
