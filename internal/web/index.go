package web

const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>龙舟赛窗口函数引擎</title>
<style>
:root{
  --bg:#0f172a; --panel:#111c33; --panel2:#16233f; --line:#24365c;
  --text:#e6edf7; --muted:#8fa3c4; --accent:#39a7ff; --accent2:#22c55e;
  --warn:#f59e0b; --err:#f87171; --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
}
*{box-sizing:border-box}
body{margin:0;background:linear-gradient(180deg,#0b1222,#0f172a 40%);color:var(--text);
  font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
header{display:flex;align-items:center;gap:16px;padding:14px 22px;border-bottom:1px solid var(--line);
  background:rgba(13,22,42,.92);position:sticky;top:0;z-index:10;backdrop-filter:blur(8px)}
header h1{font-size:17px;margin:0;font-weight:650;letter-spacing:.3px}
header .meta{color:var(--muted);font-size:12.5px}
header .spacer{flex:1}
button{background:var(--panel2);color:var(--text);border:1px solid var(--line);border-radius:8px;
  padding:7px 13px;cursor:pointer;font-size:13px;transition:.15s}
button:hover{border-color:var(--accent);color:#fff}
button.primary{background:var(--accent);border-color:var(--accent);color:#04111f;font-weight:650}
button.primary:hover{filter:brightness(1.08)}
button:disabled{opacity:.5;cursor:not-allowed}
input,select,textarea{background:#0b1528;color:var(--text);border:1px solid var(--line);
  border-radius:8px;padding:7px 10px;font-size:13px;font-family:inherit}
input:focus,textarea:focus,select:focus{outline:none;border-color:var(--accent)}
main{padding:18px 22px 60px;max-width:1500px;margin:0 auto}
.grid{display:grid;gap:16px}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:12px;overflow:hidden}
.panel-head{display:flex;align-items:center;gap:10px;padding:11px 15px;border-bottom:1px solid var(--line);
  background:var(--panel2)}
.panel-head h2{font-size:13.5px;margin:0;font-weight:650;letter-spacing:.4px;text-transform:uppercase;color:#bcd0ee}
.panel-head .hint{color:var(--muted);font-size:12px;font-weight:400;text-transform:none;letter-spacing:0}
.panel-body{padding:14px 15px}
.editor{display:flex;flex-direction:column;gap:10px}
textarea{width:100%;min-height:112px;resize:vertical;font-family:var(--mono);font-size:13px;line-height:1.55}
.controls{display:flex;flex-wrap:wrap;align-items:center;gap:10px}
.controls label{color:var(--muted);font-size:12.5px;display:flex;align-items:center;gap:6px}
.stat{display:inline-flex;align-items:center;gap:6px;padding:3px 9px;border-radius:999px;
  background:#0b1528;border:1px solid var(--line);font-family:var(--mono);font-size:12px}
.stat b{color:var(--accent);font-weight:600}
.stat.good b{color:var(--accent2)} .stat.bad b{color:var(--err)}
table{border-collapse:collapse;width:100%;font-size:12.5px}
th,td{padding:6px 10px;border-bottom:1px solid #1b2b49;text-align:left;white-space:nowrap}
th{color:#9fb6d9;font-weight:600;background:#0e1930;position:sticky;top:0}
tbody tr:hover{background:#152241}
td.num{font-variant-numeric:tabular-nums;font-family:var(--mono)}
.null{color:#5b6c8c;font-style:italic}
.scroll{overflow:auto;max-height:420px}
.scroll.tall{max-height:560px}
pre{margin:0;font-family:var(--mono);font-size:12.5px;line-height:1.55;white-space:pre-wrap;word-break:break-word}
.plan-sort{border:1px solid var(--line);border-radius:9px;margin-bottom:10px;background:#0d1930;overflow:hidden}
.plan-sort .bar{display:flex;gap:10px;align-items:center;padding:8px 11px;background:#13203a;font-size:12.5px}
.plan-sort .bar .id{font-family:var(--mono);color:var(--accent);font-weight:700}
.plan-items{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:9px;padding:10px 11px}
.plan-item{border:1px solid #1d2f52;border-radius:8px;padding:8px 10px;background:#0b1730}
.plan-item .name{font-family:var(--mono);color:#dbe8ff;font-weight:600;margin-bottom:3px}
.plan-item .row{color:var(--muted);font-size:11.8px;display:flex;gap:6px}
.plan-item .row span:first-child{min-width:62px;color:#7f95b8}
.chips{display:flex;flex-wrap:wrap;gap:7px}
.chip{font-family:var(--mono);font-size:11.8px;padding:3px 8px;border-radius:999px;background:#0c172c;
  border:1px solid var(--line);color:#a9c0e2;cursor:pointer}
.chip:hover{border-color:var(--accent);color:#fff}
.chip.active{border-color:var(--accent);color:#fff;background:#12314f}
.err{border-left:3px solid var(--err);background:#2a1420;color:#ffd9de;padding:9px 12px;border-radius:8px;
  font-family:var(--mono);font-size:12.5px;white-space:pre-wrap}
.ok{border-left:3px solid var(--accent2);background:#0f2a1c;color:#c8f5da;padding:9px 12px;border-radius:8px}
.muted{color:var(--muted)}
.two{grid-template-columns:1fr 1fr}
.three{grid-template-columns:1.15fr .85fr}
@media (max-width:1100px){.two,.three{grid-template-columns:1fr}}
.bar-track{height:7px;border-radius:4px;background:#0b1528;overflow:hidden;border:1px solid var(--line)}
.bar-fill{height:100%;background:linear-gradient(90deg,var(--accent),#7dd3fc)}
footer{padding:22px;text-align:center;color:#5f7292;font-size:12px}
kbd{font-family:var(--mono);font-size:11px;border:1px solid var(--line);border-bottom-width:2px;
  border-radius:4px;padding:1px 5px;background:#0b1528;color:#a9c0e2}
</style>
</head>
<body>
<header>
  <h1>🛶 龙舟赛窗口函数引擎</h1>
  <span class="meta" id="meta">加载中…</span>
  <span class="spacer"></span>
  <button id="btnGenerate">重新生成数据</button>
  <button id="btnVerify">抽样验证参考实现</button>
</header>
<main class="grid">

  <section class="panel">
    <div class="panel-head"><h2>数据表</h2><span class="hint" id="dataHint"></span>
      <span class="spacer" style="flex:1"></span>
      <span class="stat">显示 <b id="dataShown">–</b> / <b id="dataTotal">–</b> 行</span>
      <button id="btnPrevPage">上一页</button><button id="btnNextPage">下一页</button>
    </div>
    <div class="scroll"><table id="dataTable"></table></div>
  </section>

  <section class="panel">
    <div class="panel-head"><h2>窗口定义与查询</h2><span class="hint">输入 SQL 窗口查询，或点击下方随机生成的赛事查询</span></div>
    <div class="panel-body editor">
      <textarea id="sql" spellcheck="false"></textarea>
      <div class="controls">
        <button class="primary" id="btnRun">执行查询 <kbd>⌘⏎</kbd></button>
        <label><input type="checkbox" id="chkCheck" checked> 同时用逐行暴力扫描参考实现比对</label>
        <label>显示行数 <input id="limit" type="number" value="100" min="1" max="500" style="width:78px"></label>
        <span class="spacer" style="flex:1"></span>
        <span class="stat" id="resultStat">–</span>
      </div>
      <div id="chips" class="chips"></div>
      <div id="queryError"></div>
    </div>
  </section>

  <div class="grid two">
    <section class="panel">
      <div class="panel-head"><h2>执行计划</h2><span class="hint">共享排序按 ORDER BY 去重；聚合走线段树</span></div>
      <div class="panel-body" id="plan"><span class="muted">执行查询后显示。</span></div>
    </section>
    <section class="panel">
      <div class="panel-head"><h2>结果</h2><span class="hint" id="resultHint"></span></div>
      <div class="scroll tall" id="resultWrap"><span class="muted" style="padding:12px;display:block">执行查询后显示。</span></div>
    </section>
  </div>

  <section class="panel">
    <div class="panel-head"><h2>耗时</h2><span class="hint">引擎实际执行时间（不含网络与渲染）</span></div>
    <div class="panel-body" id="timing"><span class="muted">执行查询后显示。</span></div>
  </section>

</main>
<footer>窗口语义对齐 PostgreSQL 16 · ROWS / RANGE / GROUPS · EXCLUDE · IGNORE NULLS 扩展 · 线段树聚合</footer>

<script>
const $ = (id) => document.getElementById(id);
let state = { offset: 0, limit: 40, total: 0, currentPageQueries: [] };

function esc(s){
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}
function fmt(v){ return v === '' || v === null || v === undefined ? '<span class="null">NULL</span>' : esc(v); }

async function getJSON(url, opts){
  const r = await fetch(url, opts);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
  return data;
}

async function loadMeta(){
  const m = await getJSON('/api/meta');
  $('meta').textContent = '表 ' + m.table + ' · ' + m.rows.toLocaleString() + ' 行 · ' +
    m.columns.length + ' 列 · 随机查询 ' + m.queries + ' 条 · seed ' + m.seed;
  $('sql').value = m.defaultSQL;
  $('dataTotal').textContent = m.rows.toLocaleString();
  state.total = m.rows;
}

async function loadData(offset){
  state.offset = Math.max(0, offset);
  const d = await getJSON('/api/data?offset=' + state.offset + '&limit=' + state.limit);
  state.total = d.total;
  const head = '<thead><tr>' + d.columns.map(c =>
    '<th>' + esc(c.name) + ' <span class="muted" style="font-weight:400">' + esc(c.kind) + '</span></th>').join('') + '</tr></thead>';
  const body = '<tbody>' + d.rows.map(r =>
    '<tr>' + r.map((v, i) => {
      const isNum = d.columns[i].kind === 'int' || d.columns[i].kind === 'float' || d.columns[i].kind === 'numeric';
      return '<td class="' + (isNum ? 'num' : '') + '">' + fmt(v) + '</td>';
    }).join('') + '</tr>').join('') + '</tbody>';
  $('dataTable').innerHTML = head + body;
  $('dataShown').textContent = (state.offset + 1) + '–' + (state.offset + d.rows.length);
  $('dataHint').textContent = d.total.toLocaleString() + ' 行赛事数据（已打乱顺序，引擎需自行排序）';
}

async function loadQueries(){
  const d = await getJSON('/api/queries');
  state.currentPageQueries = d.queries;
  $('chips').innerHTML = d.queries.map((q, i) =>
    '<span class="chip" data-i="' + i + '" title="' + esc(q.features.join(', ')) + '">' +
    esc(shortSQL(q.sql)) + '</span>').join('');
  document.querySelectorAll('.chip').forEach(el => {
    el.onclick = () => {
      document.querySelectorAll('.chip').forEach(x => x.classList.remove('active'));
      el.classList.add('active');
      $('sql').value = state.currentPageQueries[+el.dataset.i].sql;
      runQuery();
    };
  });
}

function shortSQL(s){
  s = s.replace(/^SELECT /, '').replace(/ FROM races.*$/, '');
  return s.length > 92 ? s.slice(0, 92) + '…' : s;
}

let running = false;
async function runQuery(){
  if (running) return;
  running = true;
  $('btnRun').disabled = true;
  $('queryError').innerHTML = '';
  $('resultStat').textContent = '执行中…';
  try {
    const body = {
      sql: $('sql').value,
      limit: parseInt($('limit').value || '100', 10),
      checkAgainst: $('chkCheck').checked
    };
    const t0 = performance.now();
    const d = await getJSON('/api/query', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)
    });
    const wall = performance.now() - t0;
    renderResults(d);
    renderPlan(d.plan);
    renderTiming(d, wall);
  } catch (e) {
    $('resultStat').textContent = '失败';
    $('queryError').innerHTML = '<div class="err">' + esc(e.message) + '</div>';
    $('plan').innerHTML = '<span class="muted">无执行计划。</span>';
    $('resultWrap').innerHTML = '<span class="muted" style="padding:12px;display:block">无结果。</span>';
  } finally {
    running = false;
    $('btnRun').disabled = false;
  }
}

function renderResults(d){
  const head = '<thead><tr>' + d.columns.map(c =>
    '<th>' + esc(c.name) + ' <span class="muted" style="font-weight:400">' + esc(c.kind) + '</span></th>').join('') + '</tr></thead>';
  const body = '<tbody>' + d.rows.map(r =>
    '<tr>' + r.map((v, i) => {
      const k = d.columns[i].kind;
      const isNum = k === 'int' || k === 'float' || k === 'numeric';
      return '<td class="' + (isNum ? 'num' : '') + '">' + fmt(v) + '</td>';
    }).join('') + '</tr>').join('') + '</tbody>';
  $('resultWrap').innerHTML = '<table>' + head + body + '</table>';
  $('resultHint').textContent = '共 ' + d.totalRows.toLocaleString() + ' 行，显示前 ' + d.rows.length + ' 行';
  let chips = '<span class="stat">行数 <b>' + d.totalRows.toLocaleString() + '</b></span>' +
              '<span class="stat">列数 <b>' + d.columns.length + '</b></span>';
  if (d.check) {
    chips += d.check.ok
      ? '<span class="stat good">与参考实现 <b>一致</b></span>'
      : '<span class="stat bad">不一致：<b>' + esc(d.check.detail || '') + '</b></span>';
  }
  $('resultStat').innerHTML = chips;
}

function renderPlan(plan){
  if (!plan || !plan.sorts || plan.sorts.length === 0){
    $('plan').innerHTML = '<span class="muted">该查询没有窗口函数。</span>';
    return;
  }
  let html = '';
  for (const s of plan.sorts){
    const items = plan.items.filter(it => it.sortId === s.id);
    html += '<div class="plan-sort"><div class="bar">' +
      '<span class="id">' + esc(s.id) + '</span>' +
      '<span>排序一次</span>' +
      (s.partition && s.partition.length ? '<span class="muted">PARTITION BY ' + esc(s.partition.join(', ')) + '</span>' : '<span class="muted">单分区</span>') +
      (s.order && s.order.length ? '<span class="muted">ORDER BY ' + esc(s.order.join(', ')) + '</span>' : '<span class="muted">无序</span>') +
      '<span class="spacer" style="flex:1"></span>' +
      '<span class="muted">' + items.length + ' 个窗口项共享</span>' +
      '</div><div class="plan-items">' +
      items.map(it => '<div class="plan-item">' +
        '<div class="name">' + esc(it.outName) + '</div>' +
        '<div class="row"><span>函数</span><span>' + esc(it.func) + '</span></div>' +
        '<div class="row"><span>策略</span><span>' + esc(it.strategy) + '</span></div>' +
        '<div class="row"><span>帧</span><span>' + esc(it.frame) + '</span></div>' +
        '<div class="row"><span>排除</span><span>' + esc(it.exclusion) + '</span></div>' +
        '</div>').join('') +
      '</div></div>';
  }
  const uniqueSorts = plan.sorts.length;
  html += '<div class="muted" style="font-size:12px">共 ' + uniqueSorts + ' 次排序，' + plan.items.length + ' 个窗口计算项' +
    '（相同 PARTITION BY + ORDER BY 已合并）</div>';
  $('plan').innerHTML = html;
}

function renderTiming(d, wall){
  const eng = d.elapsedMs;
  const ref = d.check && d.check.referenceMs != null ? d.check.referenceMs : null;
  let html = '<div class="controls">' +
    '<span class="stat">引擎 <b>' + eng.toFixed(1) + ' ms</b></span>' +
    '<span class="stat">行数 <b>' + d.rowCount.toLocaleString() + '</b></span>' +
    '<span class="stat">吞吐 <b>' + Math.round(d.rowCount / Math.max(eng, 0.001)).toLocaleString() + '</b> 行/秒</span>';
  if (ref != null){
    html += '<span class="stat">参考实现 <b>' + ref.toFixed(1) + ' ms</b></span>';
    const ratio = ref / Math.max(eng, 0.001);
    html += '<span class="stat good">加速 <b>' + ratio.toFixed(1) + '×</b></span>';
  }
  html += '<span class="stat">请求往返 <b>' + wall.toFixed(1) + ' ms</b></span></div>';
  const pct = Math.min(100, eng / Math.max(ref || eng || 1, 0.001) * 100);
  html += '<div style="margin-top:11px"><div class="muted" style="font-size:12px;margin-bottom:5px">引擎耗时相对参考实现</div>' +
    '<div class="bar-track"><div class="bar-fill" style="width:' + pct.toFixed(1) + '%"></div></div></div>';
  $('timing').innerHTML = html;
}

async function verify(){
  $('btnVerify').disabled = true;
  $('resultStat').innerHTML = '<span class="stat">验证中…</span>';
  try {
    const d = await getJSON('/api/verify?limit=40');
    if (d.failed === 0){
      $('resultStat').innerHTML = '<span class="stat good">参考实现验证：<b>' + d.matched + '/' + d.checked + '</b> 全部一致</span>';
    } else {
      $('resultStat').innerHTML = '<span class="stat bad">验证：<b>' + d.failed + '</b> 条不一致</span>';
      $('queryError').innerHTML = '<div class="err">' + d.failures.map(f => esc(f.sql) + '\n  → ' + esc(f.error)).join('\n\n') + '</div>';
    }
  } catch (e) {
    $('resultStat').innerHTML = '<span class="stat bad">验证失败</span>';
  } finally {
    $('btnVerify').disabled = false;
  }
}

$('btnRun').onclick = runQuery;
$('btnVerify').onclick = verify;
$('btnPrevPage').onclick = () => { if (state.offset > 0) loadData(Math.max(0, state.offset - state.limit)); };
$('btnNextPage').onclick = () => { if (state.offset + state.limit < state.total) loadData(state.offset + state.limit); };
$('btnGenerate').onclick = async () => {
  $('btnGenerate').disabled = true;
  try {
    const seed = Math.floor(Math.random() * 1000000);
    await getJSON('/api/generate', {method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({rows: 3000, seed})});
    await loadMeta(); await loadData(0); await loadQueries();
    await runQuery();
  } catch (e) {
    alert('生成失败：' + e.message);
  } finally { $('btnGenerate').disabled = false; }
};
document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter'){ e.preventDefault(); runQuery(); }
});

(async function init(){
  try {
    await loadMeta();
    await Promise.all([loadData(0), loadQueries()]);
    await runQuery();
  } catch (e) {
    $('meta').textContent = '初始化失败：' + e.message;
  }
})();
</script>
</body>
</html>
`
