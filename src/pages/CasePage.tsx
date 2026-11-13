import { useEffect, useState } from 'react';
import { useWorkspaceContext } from '../App';
import { store } from '../lib/store';
import type { CaseRecord, CustodyEvent } from '../core/types';
import { formatBytes, formatOffset } from '../core/report/pdf';

export default function CasePage() {
  const api = useWorkspaceContext();
  const [cases, setCases] = useState<CaseRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [form, setForm] = useState({
    caseNumber: '',
    title: '',
    examiner: '',
    organization: '',
    notes: '',
    timezoneOffsetMinutes: 480,
  });
  const [editing, setEditing] = useState(false);

  useEffect(() => {
    store.listCases().then((list) => {
      setCases(list.sort((a, b) => b.updatedAt.localeCompare(a.updatedAt)));
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  const current = api.state.caseRecord;

  async function onCreate(e: React.FormEvent) {
    e.preventDefault();
    if (!form.caseNumber.trim() || !form.title.trim()) return;
    await api.createCase({
      caseNumber: form.caseNumber.trim(),
      title: form.title.trim(),
      examiner: form.examiner.trim(),
      organization: form.organization.trim(),
      notes: form.notes,
      timezoneOffsetMinutes: Number(form.timezoneOffsetMinutes) || 0,
    });
    setCases(await store.listCases());
    setForm({ caseNumber: '', title: '', examiner: '', organization: '', notes: '', timezoneOffsetMinutes: 480 });
  }

  async function onOpen(id: string) {
    await api.openCase(id);
    setCases(await store.listCases());
  }

  async function onSaveMeta(e: React.FormEvent) {
    e.preventDefault();
    if (!current) return;
    await api.setCaseMeta({
      caseNumber: form.caseNumber || current.caseNumber,
      title: form.title || current.title,
      examiner: form.examiner,
      organization: form.organization,
      notes: form.notes,
      timezoneOffsetMinutes: Number(form.timezoneOffsetMinutes),
    });
    setEditing(false);
    setCases(await store.listCases());
  }

  function beginEdit() {
    if (!current) return;
    setForm({
      caseNumber: current.caseNumber,
      title: current.title,
      examiner: current.examiner,
      organization: current.organization,
      notes: current.notes,
      timezoneOffsetMinutes: current.timezoneOffsetMinutes,
    });
    setEditing(true);
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>案件管理</h1>
          <p className="muted">每个案件独立保存镜像、分析结果与监管链；所有数据仅存于本机浏览器。</p>
        </div>
      </div>

      <div className="grid-2">
        <section className="card">
          <h2>{current && !editing ? '当前案件' : '新建案件'}</h2>
          {current && !editing ? (
            <div className="stack">
              <Field label="案件编号" value={current.caseNumber} />
              <Field label="案件名称" value={current.title} />
              <Field label="送检单位" value={current.organization || '—'} />
              <Field label="鉴定人" value={current.examiner || '—'} />
              <Field label="案件时区" value={formatOffset(current.timezoneOffsetMinutes)} />
              <Field label="创建时间" value={formatCn(current.createdAt)} />
              {current.notes && <Field label="备注" value={current.notes} />}
              <div className="row-actions">
                <button className="btn" onClick={beginEdit}>编辑案件信息</button>
                <button className="btn ghost" onClick={() => setEditing(true)}>新建另一案件</button>
              </div>
            </div>
          ) : (
            <form className="stack" onSubmit={editing ? onSaveMeta : onCreate}>
              <label className="field">
                <span>案件编号 *</span>
                <input required value={form.caseNumber} placeholder="如：事故鉴 2026-031"
                  onChange={(e) => setForm({ ...form, caseNumber: e.target.value })} />
              </label>
              <label className="field">
                <span>案件名称 *</span>
                <input required value={form.title} placeholder="如：某路段行车记录仪存储卡取证"
                  onChange={(e) => setForm({ ...form, title: e.target.value })} />
              </label>
              <label className="field">
                <span>送检单位</span>
                <input value={form.organization} onChange={(e) => setForm({ ...form, organization: e.target.value })} />
              </label>
              <label className="field">
                <span>鉴定人</span>
                <input value={form.examiner} onChange={(e) => setForm({ ...form, examiner: e.target.value })} />
              </label>
              <label className="field">
                <span>案件时区（FAT 时间戳解释依据）</span>
                <select value={form.timezoneOffsetMinutes}
                  onChange={(e) => setForm({ ...form, timezoneOffsetMinutes: Number(e.target.value) })}>
                  {TZ_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
                </select>
              </label>
              <label className="field">
                <span>备注</span>
                <textarea rows={3} value={form.notes} onChange={(e) => setForm({ ...form, notes: e.target.value })} />
              </label>
              <div className="row-actions">
                <button className="btn primary" type="submit">{editing ? '保存修改' : '创建案件'}</button>
                {editing && <button type="button" className="btn ghost" onClick={() => setEditing(false)}>取消</button>}
              </div>
            </form>
          )}
        </section>

        <section className="card">
          <h2>本机案件</h2>
          {loading ? (
            <p className="muted">读取中…</p>
          ) : cases.length === 0 ? (
            <p className="muted">尚无案件。创建后即可导入 dd 镜像。</p>
          ) : (
            <ul className="case-list">
              {cases.map((c) => (
                <li key={c.id} className={c.id === current?.id ? 'selected' : ''}>
                  <div>
                    <strong>{c.caseNumber}</strong>
                    <span className="muted">{c.title}</span>
                    <small>{formatCn(c.updatedAt)} · {c.examiner || '未署名'}</small>
                  </div>
                  <button className="btn small" onClick={() => onOpen(c.id)}>
                    {c.id === current?.id ? '重新载入' : '打开'}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>

      {current && (
        <section className="card">
          <h2>监管链概览</h2>
          <div className="row">
            <span className={`badge ${api.state.custodyValid === false ? 'danger' : 'ok'}`}>
              {api.state.custodyValid === null ? '未验证' : api.state.custodyValid ? '哈希链完整' : '哈希链异常'}
            </span>
            <span className="muted">共 {api.state.custodyEvents.length} 条记录</span>
            <button className="btn small ghost" onClick={() => api.refreshCustody()}>重新验证</button>
          </div>
          <CustodyTable events={api.state.custodyEvents.slice(-8)} />
        </section>
      )}

      {current && api.state.images.length > 0 && (
        <section className="card">
          <h2>本案镜像</h2>
          <table className="table">
            <thead>
              <tr><th>文件名</th><th>大小</th><th>SHA-256</th><th>状态</th></tr>
            </thead>
            <tbody>
              {api.state.images.map((img) => (
                <tr key={img.id}>
                  <td>{img.fileName}</td>
                  <td className="mono">{formatBytes(img.size)}</td>
                  <td className="mono ellipsis">{img.sha256 ?? '计算中'}</td>
                  <td>
                    <span className={`badge ${img.verification === 'verified' ? 'ok' : img.verification === 'mismatch' ? 'danger' : ''}`}>
                      {img.verification === 'verified' ? '已复核' : img.verification === 'mismatch' ? '哈希不符' : '未复核'}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
    </div>
  );
}

const TZ_OPTIONS = [
  { value: 480, label: 'UTC+08:00（中国标准时间）' },
  { value: 540, label: 'UTC+09:00（日本/韩国）' },
  { value: 0, label: 'UTC+00:00（UTC）' },
  { value: -300, label: 'UTC-05:00（美国东部）' },
  { value: -480, label: 'UTC-08:00（美国太平洋）' },
];

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="kv">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

export function CustodyTable({ events }: { events: CustodyEvent[] }) {
  if (events.length === 0) return <p className="muted">暂无记录。</p>;
  return (
    <table className="table compact">
      <thead>
        <tr><th>#</th><th>时间</th><th>操作</th><th>对象</th><th>操作人</th><th>哈希（前 12 位）</th></tr>
      </thead>
      <tbody>
        {events.map((e) => (
          <tr key={e.seq}>
            <td className="mono">{e.seq}</td>
            <td className="mono">{formatCn(e.at)}</td>
            <td>{e.actionLabel}</td>
            <td className="ellipsis">{e.target}</td>
            <td>{e.actor}</td>
            <td className="mono">{e.hash.slice(0, 12)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function formatCn(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const pad = (v: number) => String(v).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
