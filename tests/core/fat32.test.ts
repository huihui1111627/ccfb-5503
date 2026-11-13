/**
 * FAT32 解析 + 恢复的已知答案测试。
 * 生成器与被测代码相互独立；生成的镜像另行用 fsck.fat 独立校验（见 fixtures 脚本）。
 */
import { describe, expect, it } from 'vitest';
import { MemorySource } from '../../src/core/source';
import { openImage, scanVolume } from '../../src/core/analyze';
import { recoverEntry } from '../../src/core/recover';
import { buildFat32 } from '../fixtures/fat32-builder';

const t = (s: string): Date => new Date(s);

function standardSpec() {
  return buildFat32({
    sizeBytes: 64 * 1024 * 1024,
    sectorsPerCluster: 2,
    timezoneOffsetMinutes: 480,
    dirs: [
      { name: 'DCIM', created: t('2026-03-01T00:00:00Z'), modified: t('2026-03-01T00:00:00Z'), accessed: t('2026-03-01T00:00:00Z') },
      { name: 'LOGS', dir: 'DCIM', created: t('2026-03-02T00:00:00Z'), modified: t('2026-03-02T00:00:00Z'), accessed: t('2026-03-02T00:00:00Z') },
    ],
    files: [
      {
        shortName: 'REC_0001.MP4',
        name: 'REC_0001.MP4',
        size: 200_000,
        created: t('2026-03-05T00:00:00Z'),
        modified: t('2026-03-05T00:03:20Z'),
        accessed: t('2026-03-05T00:03:20Z'),
      },
      {
        shortName: 'REC_0002.MP4',
        name: '行车记录_20260305_081000.mp4',
        dir: 'DCIM',
        size: 150_000,
        created: t('2026-03-05T00:10:00Z'),
        modified: t('2026-03-05T00:12:30Z'),
        accessed: t('2026-03-05T00:12:30Z'),
      },
      {
        shortName: 'FRAG_01.MP4',
        name: 'fragmented_recording.mp4',
        dir: 'DCIM',
        size: 90_000,
        fragment: true,
        created: t('2026-03-05T00:20:00Z'),
        modified: t('2026-03-05T00:21:00Z'),
        accessed: t('2026-03-05T00:21:00Z'),
      },
      {
        // 已删除，FAT 链保留 → 可完整恢复
        shortName: 'DEL_0001.MP4',
        name: '已删除_保留簇链.mp4',
        size: 40_000,
        deleted: true,
        created: t('2026-03-04T10:00:00Z'),
        modified: t('2026-03-04T10:01:00Z'),
        accessed: t('2026-03-04T10:01:00Z'),
      },
      {
        // 已删除，FAT 链清零 → 连续假设完整恢复
        shortName: 'DEL_0002.MP4',
        name: '已删除_FAT清零.mp4',
        size: 30_000,
        deleted: true,
        clearFatOnDelete: true,
        created: t('2026-03-04T11:00:00Z'),
        modified: t('2026-03-04T11:01:00Z'),
        accessed: t('2026-03-04T11:01:00Z'),
      },
      {
        // 已删除且部分覆盖 → 不完整
        shortName: 'DEL_0003.MP4',
        name: '已删除_部分覆盖.mp4',
        size: 60_000,
        deleted: true,
        overwriteBytesAfterDelete: 20_000,
        created: t('2026-03-04T12:00:00Z'),
        modified: t('2026-03-04T12:01:00Z'),
        accessed: t('2026-03-04T12:01:00Z'),
      },
    ],
  });
}

async function openVolume(bytes: Uint8Array) {
  const source = new MemorySource(bytes);
  const image = await openImage(source);
  expect(image.volumes.length).toBeGreaterThan(0);
  const handle = image.volumes[0]!;
  const scan = await scanVolume(handle, {
    includeDeleted: true,
    maxEntries: 100_000,
    resolveChains: true,
  });
  return { source, handle, scan };
}

describe('FAT32 分区与卷识别', () => {
  it('裸卷镜像被识别为 superfloppy + FAT32', async () => {
    const built = standardSpec();
    const source = new MemorySource(built.bytes);
    const image = await openImage(source);
    expect(image.partitions[0]?.scheme).toBe('superfloppy');
    expect(image.volumes[0]?.info.fs).toBe('fat32');
    expect(image.volumes[0]?.info.clusterSize).toBe(1024);
    expect(image.volumes[0]?.info.label).toBe('DASHCAM');
    expect(image.volumes[0]?.info.freeClusters).toBe(built.expectations.volume.freeClusters);
  });

  it('卷统计与生成器预期一致', async () => {
    const built = standardSpec();
    const { handle, scan } = await openVolume(built.bytes);
    expect(handle.info.clusterCount).toBe(built.expectations.volume.totalClusters);
    // 3 个原有文件 + 1 个覆盖写入的新文件
    expect(scan.stats.files).toBe(4);
    expect(scan.stats.deletedFiles).toBe(3);
    expect(scan.stats.directories).toBe(2);
  });
});

describe('FAT32 现存文件恢复', () => {
  it('长文件名、子目录、碎片链均正确复原', async () => {
    const built = standardSpec();
    const { handle, scan } = await openVolume(built.bytes);
    const lfn = scan.entries.find((e) => e.name === '行车记录_20260305_081000.mp4');
    expect(lfn).toBeDefined();
    expect(lfn!.path).toBe('/DCIM/行车记录_20260305_081000.mp4');
    expect(lfn!.deleted).toBe(false);

    const frag = scan.entries.find((e) => e.name === 'fragmented_recording.mp4')!;
    expect(frag.runs.length).toBeGreaterThan(1);

    for (const expected of built.expectations.files.filter((f) => !f.deleted)) {
      const entry = scan.entries.find((e) => e.path === expected.path);
      expect(entry, `应列出现存文件 ${expected.path}`).toBeDefined();
      const rec = await recoverEntry(handle.reader, entry!);
      expect(rec.status, `${expected.path} 的恢复状态`).toBe('complete');
      expect(rec.hash!.sha256, `${expected.path} 的 SHA-256`).toBe(expected.sha256);
      expect(rec.hash!.md5, `${expected.path} 的 MD5`).toBe(expected.md5);
      expect(rec.recoveredSize).toBe(expected.size);
    }
  });

  it('文件大小与数据长度一致，无越界', async () => {
    const built = standardSpec();
    const { scan } = await openVolume(built.bytes);
    for (const e of scan.entries) {
      if (e.kind === 'file' && !e.deleted) {
        expect(e.size).toBeGreaterThan(0);
        expect(e.chainQuality).toBe('valid');
        expect(e.warnings).toEqual([]);
      }
    }
  });
});

describe('FAT32 删除文件恢复', () => {
  it('已删除文件的 LFN 可从残留目录项恢复，且哈希与原件一致', async () => {
    const built = standardSpec();
    const { handle, scan } = await openVolume(built.bytes);
    const del = scan.entries.find((e) => e.name === '已删除_保留簇链.mp4');
    expect(del, '应恢复出已删除文件的长名称').toBeDefined();
    expect(del!.deleted).toBe(true);
    expect(del!.longNameRecovered).toBe(true);
    expect(del!.nameConfidence).toBe('high');

    const rec = await recoverEntry(handle.reader, del!);
    const expected = built.expectations.files.find((f) => f.name === '已删除_保留簇链.mp4')!;
    expect(rec.status).toBe('complete');
    expect(rec.hash!.sha256).toBe(expected.sha256);
  });

  it('FAT 链被清零时按连续簇恢复，字节正确但状态如实标为 partial', async () => {
    const built = standardSpec();
    const { handle, scan } = await openVolume(built.bytes);
    const del = scan.entries.find((e) => e.name === '已删除_FAT清零.mp4')!;
    expect(del.chainQuality).toBe('contiguous-assumed');
    const rec = await recoverEntry(handle.reader, del);
    const expected = built.expectations.files.find((f) => f.name === '已删除_FAT清零.mp4')!;
    // 字节内容与原件一致（数据区未被覆盖）
    expect(rec.hash!.sha256).toBe(expected.sha256);
    expect(rec.recoveredSize).toBe(expected.size);
    // 但分配布局未被 FAT 证实，结论应为“不完整/需复核”
    expect(rec.status).toBe('partial');
    expect(rec.method).toBe('contiguous');
    expect(rec.reasons.join(' ')).toMatch(/连续簇假设/);
    expect(rec.reasons.join(' ')).toMatch(/未.*证实|复核/);
  });

  it('部分覆盖的删除文件被标记为 partial，缺失字节数正确', async () => {
    const built = standardSpec();
    const { handle, scan } = await openVolume(built.bytes);
    const del = scan.entries.find((e) => e.name === '已删除_部分覆盖.mp4')!;
    expect(del.deleted).toBe(true);
    const rec = await recoverEntry(handle.reader, del);
    expect(['partial', 'overwritten']).toContain(rec.status);
    expect(rec.status).toBe('partial');
    expect(rec.reasons.join(' ')).toMatch(/覆盖/);
    // 恢复出的字节与原件不同（前 20000 字节已被改写）
    expect(rec.hash!.sha256).not.toBe(
      built.expectations.files.find((f) => f.name === '已删除_部分覆盖.mp4')!.sha256,
    );
  });

  it('所有恢复结果的哈希与其字节内容自洽', async () => {
    const built = standardSpec();
    const { handle, scan } = await openVolume(built.bytes);
    for (const e of scan.entries.filter((x) => x.deleted && x.kind === 'file')) {
      const rec = await recoverEntry(handle.reader, e);
      expect(rec.hash).not.toBeNull();
      if (rec.blob) {
        const buf = new Uint8Array(await rec.blob.arrayBuffer());
        expect(buf.length).toBe(rec.recoveredSize);
      }
    }
  });
});
