/**
 * 时间线、监管链、镜像完整性测试。
 */
import { describe, expect, it } from 'vitest';
import { buildTimeline, bucketTimeline } from '../../src/core/timeline';
import {
  appendCustodyEvent,
  verifyCustodyChain,
  canonicalJson,
} from '../../src/core/custody';
import { hashSource } from '../../src/core/hash';
import { MemorySource } from '../../src/core/source';
import { buildFat32 } from '../fixtures/fat32-builder';
import type { FileEntry, CustodyEvent } from '../../src/core/types';

const ts = (iso: string, tz: number | null = null, precision: 'datetime' | 'date' = 'datetime') => ({
  raw: 'raw',
  precision,
  iso,
  tzOffsetMinutes: tz,
});

function entry(over: Partial<FileEntry>): FileEntry {
  return {
    id: 'e1', volumeId: 'v', parentPath: '/', path: '/a.mp4', name: 'a.mp4',
    shortName: null, kind: 'file', deleted: false, longNameRecovered: false,
    nameConfidence: 'exact', size: 10, firstCluster: 2, runs: [], allocatedClusters: 1,
    overwrittenClusters: 0, allocationHint: 'fat-chain', chainQuality: 'valid', attributes: 0x20,
    attributeLabels: [], times: { created: null, modified: null, accessed: null },
    entryOffset: 0, entrySpan: 1, warnings: [],
    ...over,
  };
}

describe('时间线', () => {
  it('FAT 无时区时间按案件时区归一化并标注', () => {
    const e = entry({
      times: {
        created: ts('2026-03-05T08:10:00.000'),
        modified: ts('2026-03-05T08:12:30.000'),
        accessed: null,
      },
    });
    const r = buildTimeline([e], { fatTimezoneOffsetMinutes: 480 });
    expect(r.events).toHaveLength(2);
    // 本地 08:10 +08:00 → UTC 00:10
    expect(r.events[0]!.at).toBe('2026-03-05T00:10:00.000Z');
    expect(r.events[0]!.timezoneKnown).toBe(false);
    expect(r.events[0]!.warnings.join()).toMatch(/UTC\+08:00/);
  });

  it('exFAT 显式偏移优先，不套用案件时区', () => {
    const e = entry({
      times: { created: ts('2026-03-05T08:10:00.000-05:00', -300), modified: null, accessed: null },
    });
    const r = buildTimeline([e], { fatTimezoneOffsetMinutes: 480 });
    expect(r.events[0]!.at).toBe('2026-03-05T13:10:00.000Z');
    expect(r.events[0]!.timezoneKnown).toBe(true);
  });

  it('无效时间戳被排除并计数，不污染排序', () => {
    const r = buildTimeline(
      [
        entry({ times: { created: ts('无效时间戳'), modified: null, accessed: null } }),
        entry({ id: 'e2', path: '/b.mp4', times: { created: ts('2026-01-01T00:00:00.000'), modified: null, accessed: null } }),
      ],
      { fatTimezoneOffsetMinutes: 0 },
    );
    expect(r.events).toHaveLength(1);
    expect(r.invalidTimestamps).toBe(1);
  });

  it('按时间升序排列，并可分桶', () => {
    const r = buildTimeline(
      [
        entry({ id: 'b', path: '/b.mp4', times: { created: ts('2026-03-05T10:00:00.000'), modified: null, accessed: null } }),
        entry({ id: 'a', path: '/a.mp4', times: { created: ts('2026-03-05T09:00:00.000'), modified: null, accessed: null } }),
      ],
      { fatTimezoneOffsetMinutes: 0 },
    );
    expect(r.events.map((e) => e.path)).toEqual(['/a.mp4', '/b.mp4']);
    expect(bucketTimeline(r.events, 'hour')).toEqual([{ key: '2026-03-05T09', count: 1, deleted: 0 }, { key: '2026-03-05T10', count: 1, deleted: 0 }]);
  });
});

describe('监管链', () => {
  const base = { caseId: 'C1', actor: '张三' } as const;

  it('哈希链逐条可验证，任何改动都会断链', async () => {
    const events: CustodyEvent[] = [];
    events.push(await appendCustodyEvent(events, { ...base, action: 'case.create', target: 'C1', at: '2026-03-05T00:00:00.000Z' }));
    events.push(await appendCustodyEvent(events, { ...base, action: 'image.import', target: 'card.dd', details: { size: 1024 }, at: '2026-03-05T00:01:00.000Z' }));
    events.push(await appendCustodyEvent(events, { ...base, action: 'image.hash', target: 'card.dd', details: { sha256: 'abc' }, at: '2026-03-05T00:02:00.000Z' }));

    expect((await verifyCustodyChain(events)).valid).toBe(true);

    // 篡改第 2 条的目标
    const tampered = events.map((e) => ({ ...e }));
    tampered[1] = { ...tampered[1]!, target: 'other.dd' };
    const v = await verifyCustodyChain(tampered);
    expect(v.valid).toBe(false);
    expect(v.brokenAt).toBe(2);
  });

  it('删除中间记录会被检测为序号断裂', async () => {
    const events: CustodyEvent[] = [];
    for (let i = 0; i < 4; i++) {
      events.push(await appendCustodyEvent(events, { ...base, action: 'file.scan', target: `t${i}`, at: `2026-03-05T00:0${i}:00.000Z` }));
    }
    const removed = events.filter((_, i) => i !== 1);
    const v = await verifyCustodyChain(removed);
    expect(v.valid).toBe(false);
  });

  it('canonicalJson 与键顺序无关', () => {
    expect(canonicalJson({ b: 1, a: 2 })).toBe(canonicalJson({ a: 2, b: 1 }));
  });
});

describe('镜像空值与完整性', () => {
  it('全镜像哈希两次一致（分析前后不变的前提）', async () => {
    const built = buildFat32({ sizeBytes: 8 * 1024 * 1024, sectorsPerCluster: 2, files: [] });
    const source = new MemorySource(built.bytes);
    const before = await hashSource(source);
    const after = await hashSource(source);
    expect(before).toEqual(after);
    expect(before.sha256).toBe(built.expectations.imageSha256);
    expect(before.md5).toBe(built.expectations.imageMd5);
  });
});
