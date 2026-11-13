/**
 * 时间线构建。
 *
 * 取证要点：
 *  - FAT 时间戳没有时区，按案件时区归一化，并在事件上标注“时区未知”；
 *  - exFAT 时间戳带显式 UTC 偏移，优先使用磁盘偏移；
 *  - 无效时间戳（如 1980-00-00）不进入时间线，单独计数；
 *  - 已删除条目标记，便于与现存文件对照。
 */
import type { FileEntry, TimelineEvent, TimestampValue } from './types';

export interface TimelineOptions {
  /** FAT 时间戳按此偏移解释（分钟）。exFAT 有显式偏移时忽略。 */
  fatTimezoneOffsetMinutes: number;
}

export interface TimelineResult {
  events: TimelineEvent[];
  invalidTimestamps: number;
  skippedEntries: number;
  bounds: { earliest: string | null; latest: string | null };
}

export function buildTimeline(
  entries: FileEntry[],
  opts: TimelineOptions,
): TimelineResult {
  const events: TimelineEvent[] = [];
  let invalid = 0;
  let skipped = 0;

  for (const entry of entries) {
    if (entry.kind === 'directory') {
      skipped++;
      continue;
    }
    let any = false;
    for (const kind of ['created', 'modified', 'accessed'] as const) {
      const ts = entry.times[kind];
      if (!ts) continue;
      const parsed = parseTimestamp(ts, opts.fatTimezoneOffsetMinutes);
      if (!parsed) {
        invalid++;
        continue;
      }
      any = true;
      if (parsed.dateOnly) {
        parsed.warnings.push('时间戳仅记录到日期，时刻为占位 00:00（不代表实际发生时刻）');
      }
      events.push({
        id: `${entry.id}:${kind}`,
        entryId: entry.id,
        volumeId: entry.volumeId,
        kind,
        at: parsed.iso,
        atEpoch: parsed.epoch,
        timezoneKnown: parsed.timezoneKnown,
        name: entry.name,
        path: entry.path,
        deleted: entry.deleted,
        fileKind: entry.kind,
        size: entry.size,
        warnings: parsed.warnings,
        dateOnly: parsed.dateOnly,
      });
    }
    if (!any) skipped++;
  }

  events.sort((a, b) => a.atEpoch - b.atEpoch || a.path.localeCompare(b.path));
  return {
    events,
    invalidTimestamps: invalid,
    skippedEntries: skipped,
    bounds: {
      earliest: events[0]?.at ?? null,
      latest: events[events.length - 1]?.at ?? null,
    },
  };
}

interface ParsedTimestamp {
  iso: string;
  epoch: number;
  timezoneKnown: boolean;
  dateOnly: boolean;
  warnings: string[];
}

export function parseTimestamp(
  ts: TimestampValue,
  fatTimezoneOffsetMinutes: number,
): ParsedTimestamp | null {
  if (ts.iso === '无效时间戳') return null;
  const warnings: string[] = [];
  // exFAT：iso 已带 +08:00 之类偏移，直接按偏移解析。
  if (ts.tzOffsetMinutes !== null) {
    const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})\.(\d{3})([+-]\d{2}:\d{2})?$/.exec(ts.iso);
    if (!match) return null;
    const [, y, mo, d, h, mi, s, ms, offsetPart] = match;
    const offset = offsetPart
      ? (offsetPart[0] === '-' ? -1 : 1) *
        (parseInt(offsetPart.slice(1, 3), 10) * 60 + parseInt(offsetPart.slice(4, 6), 10))
      : ts.tzOffsetMinutes;
    const utcMs = Date.UTC(
      Number(y), Number(mo) - 1, Number(d),
      Number(h), Number(mi), Number(s), Number(ms),
    );
    const epoch = utcMs - offset * 60_000;
    return {
      iso: new Date(epoch).toISOString(),
      epoch,
      timezoneKnown: true,
      dateOnly: ts.precision === 'date',
      warnings,
    };
  }
  // FAT：iso 是无时区的本地时间，按案件时区解释。
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})\.(\d{3})$/.exec(ts.iso);
  if (!match) return null;
  const [, y, mo, d, h, mi, s, ms] = match;
  const localMs = Date.UTC(
    Number(y), Number(mo) - 1, Number(d),
    Number(h), Number(mi), Number(s), Number(ms),
  );
  const epoch = localMs - fatTimezoneOffsetMinutes * 60_000;
  warnings.push(`FAT 时间戳无时区信息，按案件时区 UTC${formatOffset(fatTimezoneOffsetMinutes)} 解释`);
  return {
    iso: new Date(epoch).toISOString(),
    epoch,
    timezoneKnown: false,
    dateOnly: ts.precision === 'date',
    warnings,
  };
}

function formatOffset(minutes: number): string {
  const sign = minutes >= 0 ? '+' : '-';
  const abs = Math.abs(minutes);
  return `${sign}${String(Math.floor(abs / 60)).padStart(2, '0')}:${String(abs % 60).padStart(2, '0')}`;
}

/** 按天/小时聚合成直方图数据，供时间线页面绘制。 */
export function bucketTimeline(
  events: TimelineEvent[],
  bucket: 'hour' | 'day',
): Array<{ key: string; count: number; deleted: number }> {
  const map = new Map<string, { count: number; deleted: number }>();
  for (const e of events) {
    const key = bucket === 'hour' ? e.at.slice(0, 13) : e.at.slice(0, 10);
    const cur = map.get(key) ?? { count: 0, deleted: 0 };
    cur.count++;
    if (e.deleted) cur.deleted++;
    map.set(key, cur);
  }
  return [...map.entries()]
    .map(([key, v]) => ({ key, ...v }))
    .sort((a, b) => a.key.localeCompare(b.key));
}
