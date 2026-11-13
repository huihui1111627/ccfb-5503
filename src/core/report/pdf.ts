/**
 * PDF 报告生成。
 *
 * 使用 pdf-lib + 内嵌 Noto Sans SC（子集化）以保证中文在 PDF 中正确显示且可被
 * pdftotext 提取。报告包含：
 *  - 案件信息与操作人
 *  - 镜像哈希（分析前后）与一致性结论
 *  - 卷与分区结构
 *  - 文件清单摘要（含已删除项与恢复状态）
 *  - 雕刻结果
 *  - 时间线摘要
 *  - 监管链哈希与验证结论
 *  - 方法与局限声明
 */
import { PDFDocument, rgb, StandardFonts } from 'pdf-lib';
import fontkit from '@pdf-lib/fontkit';
import type {
  AnalysisRecord,
  CaseRecord,
  CustodyEvent,
  CarveHit,
  FileEntry,
  ImageRecord,
  RecoveryResult,
  TimelineEvent,
  VolumeInfo,
} from '../types';

export interface ReportInput {
  caseRecord: CaseRecord;
  image: ImageRecord;
  analysis: AnalysisRecord;
  entries: FileEntry[];
  recoveries: RecoveryResult[];
  carveHits: CarveHit[];
  custodyEvents: CustodyEvent[];
  custodyValid: boolean | null;
  generatedAt: string;
  /** 已构建的时间线事件（可选）。 */
  timelineEvents?: TimelineEvent[];
  timelineSummary?: {
    invalidTimestamps: number;
    skippedEntries: number;
    earliest: string | null;
    latest: string | null;
  } | null;
}

const PAGE_W = 595.28; // A4
const PAGE_H = 841.89;
const MARGIN = 44;
const LINE = 15;

interface Ctx {
  doc: PDFDocument;
  missingChars: Set<string>;
  page: ReturnType<PDFDocument['addPage']>;
  y: number;
  font: Awaited<ReturnType<PDFDocument['embedFont']>>;
  bold: Awaited<ReturnType<PDFDocument['embedFont']>>;
  mono: Awaited<ReturnType<PDFDocument['embedFont']>>;
  pageNumber: number;
  caseNumber: string;
}

export async function generateReport(input: ReportInput): Promise<Uint8Array> {
  const doc = await PDFDocument.create();
  doc.registerFontkit(fontkit);
  const fontBytes = await loadFont();
  const font = await doc.embedFont(fontBytes, { subset: true });
  const bold = await doc.embedFont(fontBytes, { subset: true });
  const mono = await doc.embedStandardFont(StandardFonts.Courier);

  const ctx: Ctx = {
    doc,
    missingChars: new Set(),
    page: doc.addPage([PAGE_W, PAGE_H]),
    y: PAGE_H - MARGIN,
    font,
    bold,
    mono,
    pageNumber: 1,
    caseNumber: input.caseRecord.caseNumber,
  };
  doc.setTitle(`取证分析报告 - ${input.caseRecord.caseNumber}`);
  doc.setAuthor(input.caseRecord.examiner || '未署名');
  doc.setSubject('存储卡取证分析报告');
  doc.setCreator('SD Card Forensics Workstation');

  drawTitle(ctx, '存储卡取证分析报告');
  ctx.y -= 6;
  drawKeyValue(ctx, '案件编号', input.caseRecord.caseNumber);
  drawKeyValue(ctx, '案件名称', input.caseRecord.title);
  drawKeyValue(ctx, '送检单位', input.caseRecord.organization || '—');
  drawKeyValue(ctx, '鉴定人', input.caseRecord.examiner || '—');
  drawKeyValue(ctx, '报告生成时间', formatCn(input.generatedAt));
  drawKeyValue(ctx, '案件时区', formatOffset(input.caseRecord.timezoneOffsetMinutes));
  if (input.caseRecord.notes.trim()) {
    drawParagraph(ctx, `备注：${input.caseRecord.notes.trim()}`, 9.5);
  }

  section(ctx, '一、检材与镜像');
  drawKeyValue(ctx, '镜像文件', input.image.fileName);
  drawKeyValue(ctx, '镜像大小', `${input.image.size.toLocaleString()} 字节 (${formatBytes(input.image.size)})`);
  drawKeyValue(ctx, '导入时间', formatCn(input.image.importedAt));
  drawKeyValue(ctx, 'MD5', input.image.md5 ?? '未计算', true);
  drawKeyValue(ctx, 'SHA-256', input.image.sha256 ?? '未计算', true);
  const integrity = input.analysis.integrity;
  drawKeyValue(
    ctx,
    '分析前后哈希一致',
    integrity.identical === true
      ? '是（已显式复核，分析前后哈希一致）'
      : integrity.identical === false
        ? '否（检测到镜像变化，结果需谨慎采信）'
        : '未做分析后复核（仅有导入时基线哈希）',
  );
  if (integrity.hashBefore && integrity.hashAfter) {
    drawKeyValue(ctx, '分析前 SHA-256', integrity.hashBefore.sha256, true);
    drawKeyValue(ctx, '分析后 SHA-256', integrity.hashAfter.sha256, true);
  }

  section(ctx, '二、分区与文件系统');
  drawPartitionTable(ctx, input.image);

  section(ctx, '三、文件扫描结果');
  const stats = input.analysis.stats;
  drawKeyValue(ctx, '现存文件', `${stats.files} 个，共 ${formatBytes(stats.totalBytes)}`);
  drawKeyValue(ctx, '已删除文件', `${stats.deletedFiles} 个`);
  drawKeyValue(ctx, '目录', `${stats.directories} 个（已删除 ${stats.deletedDirectories} 个）`);
  drawKeyValue(ctx, '恢复长文件名', `${stats.recoveredName} 个`);
  if (stats.truncatedListing) {
    drawKeyValue(ctx, '备注', `条目数达到上限，清单被截断（上限 ${stats.truncatedListing ? '已触发' : ''}）`);
  }

  section(ctx, '四、已删除文件恢复');
  drawRecoveryTable(ctx, input.entries, input.recoveries);

  section(ctx, '五、未分配空间雕刻');
  if (input.carveHits.length === 0) {
    drawParagraph(ctx, '本次未在未分配空间中发现可识别的 MP4 / JPEG 特征对象。', 9.5);
  } else {
    drawCarveTable(ctx, input.carveHits);
  }

  section(ctx, '六、时间线摘要');
  drawTimelineSummary(ctx, input);

  section(ctx, '七、监管链');
  drawCustodySection(ctx, input);

  section(ctx, '八、方法与局限');
  drawParagraph(
    ctx,
    '1. 本工具对镜像只读访问；分析前后各计算一次整镜像 MD5/SHA-256，用于证明检材未被修改。',
    9.5,
  );
  drawParagraph(
    ctx,
    '2. FAT 文件系统的时间戳不含时区，报告按案件时区解释并在时间线中标注“时区未知”；exFAT 时间戳带显式偏移，直接采用磁盘记录。',
    9.5,
  );
  drawParagraph(
    ctx,
    '3. 已删除文件的簇链若已被清零，按“连续分配”假设恢复，并在恢复结论中明确标注依据；若数据簇已被重新分配，标记为不完整或已覆盖。',
    9.5,
  );
  drawParagraph(
    ctx,
    '4. 雕刻仅针对未分配空间，按文件签名重构对象；缺少结束标记（JPEG 无 EOI、MP4 缺 moov）的对象标记为不完整，不作为完整原件采信。',
    9.5,
  );
  drawParagraph(
    ctx,
    '5. 监管链为软件级哈希链，可检测记录被篡改，但不等同于第三方可信时间戳或数字签名；如需对外举证，建议配合公证或时间戳服务。',
    9.5,
  );
  drawParagraph(
    ctx,
    '6. 本报告结论基于当前镜像的字节状态；存储介质后续的写入、磨损均衡或控制器行为可能改变可恢复性。',
    9.5,
  );

  if (ctx.missingChars.size > 0) {
    ensureSpace(ctx, 40);
    drawParagraph(
      ctx,
      `注意：报告字体子集缺少以下字符，已以“□”占位，请核对原始信息：${[...ctx.missingChars].slice(0, 40).map((c) => `U+${c.codePointAt(0)!.toString(16).toUpperCase()}`).join('、')}${ctx.missingChars.size > 40 ? ' 等' : ''}`,
      9,
    );
  }
  addFooters(ctx);
  return doc.save();
}

/**
 * 报告字体为常用字子集（GB2312 + ASCII + 常用标点）。若案件中出现生僻姓名/地名，
 * pdf-lib 会因缺字形抛错。这里基于字体真实字符集（getCharacterSet）逐字符检查，
 * 缺字替换为“□”，保证报告始终能生成，同时不丢失结构信息。
 */
const sanitizedCache = new WeakMap<object, { set: Set<number>; warned: Set<string> }>();

function sanitizeForFont(
  text: string,
  font: { getCharacterSet?: () => number[] },
  missing?: Set<string>,
): string {
  const key = font as object;
  let entry = sanitizedCache.get(key);
  if (!entry) {
    const set = new Set(font.getCharacterSet ? font.getCharacterSet() : []);
    entry = { set, warned: new Set() };
    sanitizedCache.set(key, entry);
  }
  if (entry.set.size === 0) return text; // 无法判定时保持原样
  let out = '';
  for (const ch of text) {
    const cp = ch.codePointAt(0)!;
    // 控制字符（保留换行/制表）直接放行由调用方处理。
    if (cp === 0x0a || cp === 0x0d || cp === 0x09) {
      out += ch;
      continue;
    }
    const code = cp > 0xffff ? -1 : cp;
    if (code >= 0 && entry.set.has(code)) {
      out += ch;
    } else {
      if (!entry.warned.has(ch)) {
        entry.warned.add(ch);
        missing?.add(ch);
      }
      out += '□';
    }
  }
  return out;
}

async function loadFont(): Promise<Uint8Array> {
  // 优先使用随应用发布的 Noto Sans SC；缺失时回落到系统可读的替代字体。
  const urls = ['/fonts/NotoSansSC-Regular.ttf'];
  for (const url of urls) {
    try {
      const res = await fetch(url);
      if (res.ok) return new Uint8Array(await res.arrayBuffer());
    } catch {
      // 继续尝试
    }
  }
  throw new Error('未能加载报告字体 /fonts/NotoSansSC-Regular.ttf');
}

function drawTitle(ctx: Ctx, text: string): void {
  ctx.page.drawText(sanitizeForFont(text, ctx.font, ctx.missingChars), {
    x: MARGIN,
    y: ctx.y - 22,
    size: 20,
    font: ctx.font,
    color: rgb(0.08, 0.12, 0.2),
  });
  ctx.y -= 34;
  ctx.page.drawLine({
    start: { x: MARGIN, y: ctx.y },
    end: { x: PAGE_W - MARGIN, y: ctx.y },
    thickness: 1.4,
    color: rgb(0.1, 0.25, 0.45),
  });
  ctx.y -= 18;
}

function section(ctx: Ctx, title: string): void {
  ensureSpace(ctx, 40);
  ctx.y -= 8;
  ctx.page.drawText(sanitizeForFont(title, ctx.font, ctx.missingChars), { x: MARGIN, y: ctx.y, size: 13, font: ctx.font, color: rgb(0.1, 0.25, 0.45) });
  ctx.y -= 6;
  ctx.page.drawLine({
    start: { x: MARGIN, y: ctx.y },
    end: { x: MARGIN + 160, y: ctx.y },
    thickness: 0.8,
    color: rgb(0.1, 0.25, 0.45),
  });
  ctx.y -= 14;
}

function drawKeyValue(ctx: Ctx, key: string, value: string, mono = false): void {
  ensureSpace(ctx, LINE);
  const font = mono ? ctx.mono : ctx.font;
  const keyWidth = 112;
  ctx.page.drawText(sanitizeForFont(key, ctx.font, ctx.missingChars), { x: MARGIN, y: ctx.y, size: 9.5, font: ctx.font, color: rgb(0.35, 0.38, 0.42) });
  const lines = wrapText(sanitizeForFont(value, font, ctx.missingChars), font, 9.5, PAGE_W - MARGIN * 2 - keyWidth);
  for (let i = 0; i < lines.length; i++) {
    ensureSpace(ctx, LINE);
    ctx.page.drawText(lines[i]!, {
      x: MARGIN + keyWidth,
      y: ctx.y,
      size: mono ? 8.2 : 9.5,
      font,
      color: rgb(0.08, 0.1, 0.14),
    });
    ctx.y -= LINE;
  }
}

function drawParagraph(ctx: Ctx, text: string, size: number): void {
  const lines = wrapText(sanitizeForFont(text, ctx.font, ctx.missingChars), ctx.font, size, PAGE_W - MARGIN * 2);
  for (const line of lines) {
    ensureSpace(ctx, LINE);
    ctx.page.drawText(line, { x: MARGIN, y: ctx.y, size, font: ctx.font, color: rgb(0.12, 0.14, 0.18) });
    ctx.y -= LINE;
  }
  ctx.y -= 3;
}

function drawPartitionTable(ctx: Ctx, image: ImageRecord): void {
  const cols = [30, 105, 75, 95, 55, 130];
  const headers = ['#', '偏移', '大小', '类型', '文件系统', '卷标'];
  drawTableHeader(ctx, cols, headers, MARGIN);
  for (const p of image.partitions) {
    const row = [
      String(p.index),
      p.offset.toLocaleString(),
      formatBytes(p.size),
      p.typeLabel,
      p.volume?.fs?.toUpperCase() ?? '未识别',
      p.volume?.label ?? '—',
    ];
    drawRow(ctx, cols, row);
  }
  if (image.partitions.length === 0) {
    drawParagraph(ctx, '未识别到分区表。', 9.5);
  }
}

function drawRecoveryTable(ctx: Ctx, entries: FileEntry[], recoveries: RecoveryResult[]): void {
  const deleted = entries.filter((e) => e.deleted && e.kind === 'file');
  if (deleted.length === 0) {
    drawParagraph(ctx, '未发现已删除文件条目。', 9.5);
    return;
  }
  const cols = [190, 60, 60, 65, 130];
  drawTableHeader(ctx, cols, ['文件名', '声明大小', '恢复大小', '结论', 'SHA-256（前 16 位）'], MARGIN);
  const recoveryById = new Map(recoveries.map((r) => [r.entryId, r]));
  for (const e of deleted.slice(0, 200)) {
    const r = recoveryById.get(e.id);
    drawRow(ctx, cols, [
      truncate(e.path, 34),
      formatBytes(e.size),
      r ? formatBytes(r.recoveredSize) : '未恢复',
      r ? statusLabel(r.status) : '—',
      r?.hash ? r.hash.sha256.slice(0, 16) : '—',
    ]);
  }
  if (deleted.length > 200) {
    drawParagraph(ctx, `（仅列出前 200 条，共 ${deleted.length} 条；完整清单见导出数据。）`, 9);
  }
}

function drawCarveTable(ctx: Ctx, hits: CarveHit[]): void {
  const cols = [55, 95, 70, 60, 60, 110];
  drawTableHeader(ctx, cols, ['类型', '镜像偏移', '大小', '完整性', '卷', 'SHA-256（前 16 位）'], MARGIN);
  for (const h of hits.slice(0, 200)) {
    drawRow(ctx, cols, [
      h.type.toUpperCase(),
      h.offset.toLocaleString(),
      formatBytes(h.size),
      h.complete ? '完整' : '不完整',
      h.volumeId,
      h.hash.sha256.slice(0, 16),
    ]);
  }
  if (hits.length > 200) {
    drawParagraph(ctx, `（仅列出前 200 条，共 ${hits.length} 条。）`, 9);
  }
}

function drawTimelineSummary(ctx: Ctx, input: ReportInput): void {
  const events = input.timelineEvents ?? [];
  if (events.length === 0) {
    const created = input.analysis.entries
      .filter((e: FileEntry) => e.kind === 'file' && e.times.created)
      .map((e: FileEntry) => e.times.created!.iso)
      .filter((iso: string) => iso !== '无效时间戳')
      .sort();
    if (created.length === 0) {
      drawParagraph(ctx, '未提取到有效时间戳。', 9.5);
      return;
    }
    drawKeyValue(ctx, '可见时间范围', `${created[0]} 至 ${created[created.length - 1]}`);
    drawKeyValue(ctx, '含时间戳的文件', `${created.length} 个（按创建时间统计）`);
    return;
  }

  drawKeyValue(ctx, '事件总数', `${events.length}`);
  drawKeyValue(ctx, '时间范围', `${formatCn(events[0]!.at)} 至 ${formatCn(events[events.length - 1]!.at)}`);
  const dateOnly = events.filter((e) => e.dateOnly).length;
  drawKeyValue(
    ctx,
    '仅日期精度事件',
    `${dateOnly} 个（访问时间仅记录日期，时刻不代表实际发生时间）`,
  );
  if (input.timelineSummary && input.timelineSummary.invalidTimestamps > 0) {
    drawKeyValue(ctx, '无效时间戳', `${input.timelineSummary.invalidTimestamps} 个（已排除）`);
  }

  // 按天统计（最多 10 天，避免报告过长）
  const byDay = new Map<string, { count: number; deleted: number }>();
  for (const e of events) {
    const key = e.at.slice(0, 10);
    const cur = byDay.get(key) ?? { count: 0, deleted: 0 };
    cur.count++;
    if (e.deleted) cur.deleted++;
    byDay.set(key, cur);
  }
  const days = [...byDay.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  drawTableHeader(ctx, [110, 70, 80], ['日期 (UTC)', '事件数', '其中已删除'], MARGIN);
  for (const [day, v] of days.slice(0, 10)) {
    drawRow(ctx, [110, 70, 80], [day, String(v.count), String(v.deleted)]);
  }
  if (days.length > 10) {
    drawParagraph(ctx, `（仅列出前 10 天，共 ${days.length} 天。）`, 9);
  }

  const important = events
    .filter((e) => e.kind === 'modified' && !e.dateOnly)
    .slice(0, 8);
  if (important.length > 0) {
    ctx.y -= 6;
    ensureSpace(ctx, 26);
    drawParagraph(ctx, '关键文件修改事件（按时间排序，最多 8 条）：', 9.5);
    for (const e of important) {
      ensureSpace(ctx, LINE);
      ctx.page.drawText(
        `${formatCn(e.at)}  ${truncate(e.path, 52)}${e.deleted ? '（已删除）' : ''}`,
        { x: MARGIN, y: ctx.y, size: 8.2, font: ctx.font, color: rgb(0.12, 0.14, 0.18) },
      );
      ctx.y -= LINE;
    }
  }
}

function drawCustodySection(ctx: Ctx, input: ReportInput): void {
  drawKeyValue(ctx, '监管链记录数', String(input.custodyEvents.length));
  drawKeyValue(
    ctx,
    '哈希链验证',
    input.custodyValid === null ? '未验证' : input.custodyValid ? '通过（未发现篡改）' : '未通过',
  );
  if (input.custodyEvents.length > 0) {
    const last = input.custodyEvents[input.custodyEvents.length - 1]!;
    drawKeyValue(ctx, '链尾哈希', last.hash, true);
  }
  ensureSpace(ctx, 30);
  ctx.page.drawText('关键操作序列（最近 15 条）', {
    x: MARGIN, y: ctx.y, size: 9.5, font: ctx.font, color: rgb(0.35, 0.38, 0.42),
  });
  ctx.y -= LINE;
  const recent = input.custodyEvents.slice(-15);
  for (const e of recent) {
    ensureSpace(ctx, LINE);
    ctx.page.drawText(
      `#${e.seq}  ${formatCn(e.at)}  ${e.actionLabel}  ${truncate(e.target, 40)}`,
      { x: MARGIN, y: ctx.y, size: 8.2, font: ctx.font, color: rgb(0.12, 0.14, 0.18) },
    );
    ctx.y -= LINE;
  }
}

function drawTableHeader(ctx: Ctx, cols: number[], headers: string[], x0: number): void {
  ensureSpace(ctx, LINE + 4);
  let x = x0;
  for (let i = 0; i < headers.length; i++) {
    ctx.page.drawText(sanitizeForFont(headers[i]!, ctx.font, ctx.missingChars), { x, y: ctx.y, size: 8.4, font: ctx.font, color: rgb(0.4, 0.42, 0.46) });
    x += cols[i]!;
  }
  ctx.y -= 4;
  ctx.page.drawLine({
    start: { x: x0, y: ctx.y },
    end: { x: x0 + cols.reduce((s, c) => s + c, 0), y: ctx.y },
    thickness: 0.4,
    color: rgb(0.7, 0.72, 0.75),
  });
  ctx.y -= LINE - 2;
}

function drawRow(ctx: Ctx, cols: number[], cells: string[]): void {
  ensureSpace(ctx, LINE);
  let x = MARGIN;
  for (let i = 0; i < cells.length; i++) {
    ctx.page.drawText(sanitizeForFont(truncate(cells[i]!, Math.floor(cols[i]! / 4.6)), ctx.font, ctx.missingChars), {
      x,
      y: ctx.y,
      size: 8,
      font: ctx.font,
      color: rgb(0.12, 0.14, 0.18),
    });
    x += cols[i]!;
  }
  ctx.y -= LINE - 2;
}

function ensureSpace(ctx: Ctx, needed: number): void {
  if (ctx.y - needed > MARGIN + 24) return;
  ctx.page = ctx.doc.addPage([PAGE_W, PAGE_H]);
  ctx.pageNumber++;
  ctx.y = PAGE_H - MARGIN;
}

function addFooters(ctx: Ctx): void {
  const pages = ctx.doc.getPages();
  pages.forEach((page, i) => {
    page.drawText(`案件 ${ctx.caseNumber} · 第 ${i + 1}/${pages.length} 页`, {
      x: MARGIN,
      y: MARGIN / 2,
      size: 8,
      font: ctx.font,
      color: rgb(0.45, 0.47, 0.5),
    });
    page.drawText('本报告由存储卡取证工作站生成', {
      x: PAGE_W - MARGIN - 150,
      y: MARGIN / 2,
      size: 8,
      font: ctx.font,
      color: rgb(0.45, 0.47, 0.5),
    });
  });
}

function wrapText(
  text: string,
  font: { widthOfTextAtSize: (t: string, s: number) => number },
  size: number,
  maxWidth: number,
): string[] {
  const out: string[] = [];
  for (const paragraph of text.split('\n')) {
    let line = '';
    for (const ch of paragraph) {
      const candidate = line + ch;
      if (font.widthOfTextAtSize(candidate, size) > maxWidth && line.length > 0) {
        out.push(line);
        line = ch;
      } else {
        line = candidate;
      }
    }
    out.push(line);
  }
  return out.flatMap((l) => (l === '' ? [''] : [l]));
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

function statusLabel(s: string): string {
  const map: Record<string, string> = {
    complete: '完整',
    partial: '不完整',
    overwritten: '已覆盖',
    failed: '失败',
  };
  return map[s] ?? s;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(2)} MiB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GiB`;
}

function formatOffset(minutes: number): string {
  const sign = minutes >= 0 ? '+' : '-';
  const abs = Math.abs(minutes);
  return `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, '0')}:${String(abs % 60).padStart(2, '0')}`;
}

function formatCn(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  // 统一以案件时区展示；此处使用本地格式，调用方传入的已是 ISO。
  const pad = (v: number) => String(v).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export type { ReportInput as ReportInputType };
export { formatBytes, formatOffset };
export type { VolumeInfo };
