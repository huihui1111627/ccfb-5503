/**
 * exFAT 解析与恢复的已知答案测试。
 */
import { describe, expect, it } from 'vitest';
import { MemorySource } from '../../src/core/source';
import { openImage, scanVolume } from '../../src/core/analyze';
import { recoverEntry } from '../../src/core/recover';
import { buildExfat } from '../fixtures/exfat-builder';

const t = (s: string): Date => new Date(s);

function spec() {
  return buildExfat({
    sizeBytes: 32 * 1024 * 1024,
    clusterSize: 4096,
    timezoneOffsetMinutes: 480,
    dirs: [
      { name: 'DCIM', created: t('2026-03-01T00:00:00Z'), modified: t('2026-03-01T00:00:00Z'), accessed: t('2026-03-01T00:00:00Z') },
    ],
    files: [
      {
        name: '20260305_081000.MP4',
        size: 300_000,
        created: t('2026-03-05T00:10:00Z'),
        modified: t('2026-03-05T00:12:30Z'),
        accessed: t('2026-03-05T00:12:30Z'),
      },
      {
        name: '行车记录仪_20260305_082000.mp4',
        dir: 'DCIM',
        size: 150_000,
        chained: true,
        created: t('2026-03-05T00:20:00Z'),
        modified: t('2026-03-05T00:23:00Z'),
        accessed: t('2026-03-05T00:23:00Z'),
      },
      {
        name: '已删除_连续.mp4',
        size: 80_000,
        deleted: true,
        created: t('2026-03-04T09:00:00Z'),
        modified: t('2026-03-04T09:02:00Z'),
        accessed: t('2026-03-04T09:02:00Z'),
      },
      {
        name: '已删除_部分覆盖.mp4',
        size: 60_000,
        deleted: true,
        overwriteBytesAfterDelete: 20_000,
        created: t('2026-03-04T10:00:00Z'),
        modified: t('2026-03-04T10:01:00Z'),
        accessed: t('2026-03-04T10:01:00Z'),
      },
    ],
  });
}

async function open(bytes: Uint8Array) {
  const image = await openImage(new MemorySource(bytes));
  const handle = image.volumes[0]!;
  const scan = await scanVolume(handle, {
    includeDeleted: true,
    maxEntries: 100_000,
    resolveChains: true,
  });
  return { image, handle, scan };
}

describe('exFAT 卷识别', () => {
  it('引导区校验和、卷标、簇参数正确', async () => {
    const built = spec();
    const { handle, image } = await open(built.bytes);
    expect(image.partitions[0]?.scheme).toBe('superfloppy');
    expect(handle.info.fs).toBe('exfat');
    expect(handle.info.label).toBe('DASHEX');
    expect(handle.info.clusterSize).toBe(4096);
    expect(handle.info.clusterCount).toBe(built.expectations.volume.totalClusters);
    expect(handle.info.backupBootValid).toBe(true);
    expect(handle.info.warnings.filter((w) => w.code === 'boot-checksum')).toEqual([]);
  });

  it('现存与已删除条目均能列出，名称置信度 exact/high', async () => {
    const built = spec();
    const { scan } = await open(built.bytes);
    expect(scan.stats.files).toBe(2);
    expect(scan.stats.deletedFiles).toBe(2);
    const deleted = scan.entries.find((e) => e.name === '已删除_连续.mp4');
    expect(deleted).toBeDefined();
    expect(deleted!.nameConfidence).toBe('high');
  });
});

describe('exFAT 恢复', () => {
  it('连续分配与链式分配的文件均逐字节还原', async () => {
    const built = spec();
    const { handle, scan } = await open(built.bytes);
    for (const expected of built.expectations.files.filter((f) => !f.deleted)) {
      const entry = scan.entries.find((e) => e.path === expected.path)!;
      expect(entry).toBeDefined();
      const rec = await recoverEntry(handle.reader, entry);
      expect(rec.status).toBe('complete');
      expect(rec.hash!.sha256).toBe(expected.sha256);
      expect(rec.hash!.md5).toBe(expected.md5);
    }
  });

  it('已删除连续文件按 NoFatChain 恢复，未被覆盖时判定完整', async () => {
    const built = spec();
    const { handle, scan } = await open(built.bytes);
    const del = scan.entries.find((e) => e.name === '已删除_连续.mp4')!;
    const rec = await recoverEntry(handle.reader, del);
    const expected = built.expectations.files.find((f) => f.name === '已删除_连续.mp4')!;
    expect(rec.status).toBe('complete');
    expect(rec.hash!.sha256).toBe(expected.sha256);
    expect(['exfat-contiguous', 'exfat-chain']).toContain(rec.method);
  });

  it('已删除且部分簇被重新分配，按分配位图判定为 partial/overwritten', async () => {
    const built = spec();
    const { handle, scan } = await open(built.bytes);
    const del = scan.entries.find((e) => e.name === '已删除_部分覆盖.mp4')!;
    const rec = await recoverEntry(handle.reader, del);
    expect(['partial', 'overwritten']).toContain(rec.status);
    expect(rec.overwrittenClusters).toBeGreaterThan(0);
  });

  it('exFAT 时间戳带显式 UTC 偏移，还原为正确 UTC 时刻', async () => {
    const built = spec();
    const { scan } = await open(built.bytes);
    const entry = scan.entries.find((e) => e.name === '20260305_081000.MP4')!;
    expect(entry.times.created?.tzOffsetMinutes).toBe(480);
    expect(entry.times.created?.iso).toBe('2026-03-05T08:10:00.000+08:00');
  });
});
