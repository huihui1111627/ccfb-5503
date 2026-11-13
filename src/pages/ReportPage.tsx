import { useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useWorkspaceContext } from '../App';
import { formatBytes, formatOffset } from '../core/report/pdf';
import { verifyCustodyChain } from '../core/custody';
import type { AnalysisRecord, CarveHit, RecoveryResult } from '../core/types';
import { EmptyState } from './AnalysisPage';
import { custody } from '../lib/custodyService';
import { store } from '../lib/store';

export default function ReportPage() {
  const api = useWorkspaceContext();
  const navigate = useNavigate();
  const [generating, setGenerating] = useState(false);
  const [status, setStatus] = useState<string | null>(null);
  const [custodyJson, setCustodyJson] = useState<string | null>(null);

  const c = api.state.caseRecord;
  const image = api.state.activeImage;
  const analysis = api.state.analysis;
  const stats = api.state.stats;

  const summary = useMemo(() => {
    const deleted = api.state.entries.filter((e) => e.deleted && e.kind === 'file');
    const complete = api.state.recoveries.filter((r) => r.status === 'complete').length;
    const partial = api.state.recoveries.filter((r) => r.status === 'partial').length;
    const overwritten = api.state.recoveries.filter((r) => r.status === 'overwritten').length;
    return { deleted: deleted.length, complete, partial, overwritten };
  }, [api.state.entries, api.state.recoveries]);

  if (!c || !image || !analysis || !stats) {
    return (
      <div className="page">
        <EmptyState
          title="生成报告前需要：案件、镜像与一次完整扫描"
          action={<button className="btn primary" onClick={() => navigate('/analysis')}>前往镜像分析</button>}
        />
      </div>
    );
  }

  async function onGenerate() {
    setGenerating(true);
    setStatus('正在整理数据与排版…');
    try {
      const analysisRecord: AnalysisRecord = {
        id: 'report',
        caseId: c!.id,
        imageId: image!.id,
        createdAt: new Date().toISOString(),
        volumes: analysis!.volumes,
        entries: api.state.entries,
        stats: stats!,
        carveHits: api.state.carveHits as unknown as CarveHit[],
        // 只在“显式复核且一致”时给出哈希不变的结论；否则如实记为未复核。
        integrity: {
          hashBefore: image!.md5 && image!.sha256 ? { md5: image!.md5, sha256: image!.sha256 } : null,
          hashAfter: image!.verification === 'verified' && image!.md5 && image!.sha256
            ? { md5: image!.md5, sha256: image!.sha256 }
            : null,
          identical: image!.verification === 'verified' ? true : null,
          verifiedAt: image!.lastVerifiedAt,
        },
      };
      const recoveryResults: RecoveryResult[] = api.state.recoveries.map((r) => {
        const { blobUrl: _blobUrl, ...rest } = r;
        void _blobUrl;
        return rest;
      });
      const events = await store.listCustody(c!.id);
      const verification = await verifyCustodyChain(events);
      await custody.append(c!, 'report.generate', `报告-${c!.caseNumber}`, {
        files: stats!.files, deleted: stats!.deletedFiles, carveHits: api.state.carveHits.length,
      });
      // 报告引擎（pdf-lib）体积较大，按需加载，避免拖慢首屏。
      const { generateReport } = await import('../core/report/pdf');
      const bytes = await generateReport({
        caseRecord: c!,
        image: image!,
        analysis: analysisRecord,
        entries: api.state.entries,
        recoveries: recoveryResults,
        carveHits: api.state.carveHits as unknown as CarveHit[],
        custodyEvents: [...events, ...custody.current.filter((e) => !events.some((x) => x.seq === e.seq))],
        custodyValid: verification.valid,
        generatedAt: new Date().toISOString(),
        ...(api.state.timeline ? {
          timelineEvents: api.state.timeline.events,
          timelineSummary: {
            invalidTimestamps: api.state.timeline.invalidTimestamps,
            skippedEntries: api.state.timeline.skippedEntries,
            earliest: api.state.timeline.bounds.earliest,
            latest: api.state.timeline.bounds.latest,
          },
        } : {}),
      });
      const blob = new Blob([bytes as unknown as BlobPart], { type: 'application/pdf' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `${c!.caseNumber || 'report'}-取证分析报告.pdf`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 10_000);
      await api.refreshCustody();
      setStatus('报告已生成并开始下载。');
    } catch (err) {
      setStatus(`生成失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setGenerating(false);
    }
  }

  async function onExportCustody() {
    const events = await store.listCustody(c!.id);
    setCustodyJson(custody.exportJson(c!));
    const blob = new Blob([custody.exportJson(c!)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `${c!.caseNumber || 'case'}-监管链.json`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
    await custody.append(c!, 'custody.export', `${events.length} 条记录`, {});
    await api.refreshCustody();
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>报告与监管链</h1>
          <p className="muted">导出 PDF 报告；监管链可独立导出并复核哈希链。</p>
        </div>
        <div className="row">
          <button className="btn ghost" onClick={onExportCustody}>导出监管链 JSON</button>
          <button className="btn primary" onClick={onGenerate} disabled={generating || !!api.state.busy}>
            {generating ? '生成中…' : '生成 PDF 报告'}
          </button>
        </div>
      </div>

      {status && <div className="notice">{status}</div>}

      <div className="grid-2">
        <section className="card">
          <h2>报告预览</h2>
          <div className="report-preview">
            <h3>存储卡取证分析报告</h3>
            <dl>
              <dt>案件编号</dt><dd>{c.caseNumber}</dd>
              <dt>案件名称</dt><dd>{c.title}</dd>
              <dt>鉴定人</dt><dd>{c.examiner || '未署名'}</dd>
              <dt>送检单位</dt><dd>{c.organization || '—'}</dd>
              <dt>案件时区</dt><dd>{formatOffset(c.timezoneOffsetMinutes)}</dd>
              <dt>镜像</dt><dd>{image.fileName}（{formatBytes(image.size)}）</dd>
              <dt>MD5</dt><dd className="mono">{image.md5 ?? '—'}</dd>
              <dt>SHA-256</dt><dd className="mono break">{image.sha256 ?? '—'}</dd>
              <dt>分析前后一致</dt>
              <dd>{image.verification === 'verified' ? '已复核一致' : image.verification === 'mismatch' ? '不一致（需谨慎）' : '仅记录导入哈希，未做分析后复核'}</dd>
            </dl>
          </div>
          <p className="muted">
            报告包含：镜像哈希与完整性、分区/卷结构、文件统计、已删除文件恢复结论、雕刻结果、时间线摘要、监管链与局限性声明。
          </p>
        </section>

        <section className="card">
          <h2>数据摘要</h2>
          <div className="grid-2 tight">
            <Summary label="现存文件" value={`${stats.files}`} />
            <Summary label="已删除文件" value={`${stats.deletedFiles}`} />
            <Summary label="已恢复（完整）" value={`${summary.complete}`} tone="ok" />
            <Summary label="已恢复（不完整）" value={`${summary.partial}`} tone="warn" />
            <Summary label="已覆盖" value={`${summary.overwritten}`} tone="danger" />
            <Summary label="雕刻命中" value={`${api.state.carveHits.length}`} />
            <Summary label="时间线事件" value={`${api.state.timeline?.events.length ?? 0}`} />
            <Summary label="监管链记录" value={`${api.state.custodyEvents.length}`} />
          </div>
          {!api.state.timeline && (
            <p className="muted">提示：先构建时间线可让报告包含完整的事件统计。</p>
          )}
          {api.state.carveHits.length === 0 && (
            <p className="muted">提示：尚未执行雕刻；报告将注明未发现可识别对象。</p>
          )}
          <div className="row" style={{ marginTop: 12 }}>
            <span className={`badge ${api.state.custodyValid === false ? 'danger' : 'ok'}`}>
              {api.state.custodyValid === null ? '监管链未验证' : api.state.custodyValid ? '监管链完整' : '监管链异常'}
            </span>
            <span className="muted">链尾 {api.state.custodyEvents.at(-1)?.hash.slice(0, 16) ?? '—'}…</span>
          </div>
        </section>
      </div>

      <section className="card">
        <h2>监管链完整记录</h2>
        <div className="table-scroll tall">
          <table className="table compact">
            <thead>
              <tr><th>#</th><th>时间</th><th>操作</th><th>对象</th><th>操作人</th><th>前序哈希</th><th>本记录哈希</th></tr>
            </thead>
            <tbody>
              {api.state.custodyEvents.map((e) => (
                <tr key={e.seq}>
                  <td className="mono">{e.seq}</td>
                  <td className="mono">{new Date(e.at).toLocaleString('zh-CN', { hour12: false })}</td>
                  <td>{e.actionLabel}</td>
                  <td className="ellipsis" title={e.target}>{e.target}</td>
                  <td>{e.actor}</td>
                  <td className="mono ellipsis">{e.prevHash.slice(0, 12)}</td>
                  <td className="mono ellipsis">{e.hash.slice(0, 12)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      {custodyJson && (
        <section className="card">
          <h2>导出的监管链 JSON（片段）</h2>
          <pre className="code">{custodyJson.slice(0, 2000)}{custodyJson.length > 2000 ? '\n…' : ''}</pre>
        </section>
      )}
    </div>
  );
}

function Summary({ label, value, tone }: { label: string; value: string; tone?: 'ok' | 'warn' | 'danger' }) {
  return (
    <div className={`summary ${tone ?? ''}`}>
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}
