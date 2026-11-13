/**
 * 只读性与完整性回归测试。
 *
 * 取证工具最重要的非功能保证：对镜像的任何分析都不能改动原始字节。
 * 这里把“完整流程”（识别 → 扫描 → 恢复 → 雕刻 → 时间线）跑一遍，
 * 用哈希对比前后镜像，验证工具确实只读。
 */
import { describe, expect, it } from 'vitest';
import { MemorySource } from '../../src/core/source';
import { openImage, scanVolume, carveVolume } from '../../src/core/analyze';
import { recoverEntry } from '../../src/core/recover';
import { buildTimeline } from '../../src/core/timeline';
import { hashSource } from '../../src/core/hash';
import { buildFat32 } from '../fixtures/fat32-builder';
import { buildExfat } from '../fixtures/exfat-builder';
import { sha256Hex } from '../fixtures/common';
import { makeMp4, tinyJpeg } from '../fixtures/media';

const at = (s: string): Date => new Date(s);
const stamp = (s: string) => ({ created: at(s), modified: at(s), accessed: at(s) });

describe('完整流程只读性', () => {
  it('FAT32：扫描+恢复+雕刻+时间线后镜像哈希不变', async () => {
    const jpeg = tinyJpeg();
    const built = buildFat32({
      sizeBytes: 16 * 1024 * 1024,
      sectorsPerCluster: 8,
      files: [
        { shortName: 'KEEP.MP4', name: '保留.mp4', size: jpeg.length, content: jpeg, ...stamp('2026-03-05T00:00:00Z') },
        {
          shortName: 'DEL.JPG', name: '已删除.jpg', size: jpeg.length, content: jpeg,
          deleted: true, clearFatOnDelete: true, ...stamp('2026-03-04T00:00:00Z'),
        },
      ],
    });
    const source = new MemorySource(built.bytes);
    const before = await hashSource(source);
    const opened = await openImage(source);
    const handle = opened.volumes[0]!;
    const scan = await scanVolume(handle, { includeDeleted: true, maxEntries: 10_000, resolveChains: true });
    for (const e of scan.entries.filter((x) => x.kind === 'file')) {
      await recoverEntry(handle.reader, e);
    }
    await carveVolume(source, handle, scan.entries, {
      maxHits: 50, maxObjectSize: 4 * 1024 * 1024, minJpegSize: 64, minMp4Size: 64,
    });
    buildTimeline(scan.entries, { fatTimezoneOffsetMinutes: 480 });
    const after = await hashSource(source);
    expect(after).toEqual(before);
    // 同时确认底层字节未变（防止读取器意外原地修改 subarray）
    expect(sha256Hex(built.bytes)).toBe(built.expectations.imageSha256);
  });

  it('exFAT：扫描+恢复后镜像哈希不变', async () => {
    const mp4 = makeMp4(1);
    if (!mp4) return;
    const built = buildExfat({
      sizeBytes: 16 * 1024 * 1024,
      clusterSize: 4096,
      files: [
        { name: 'keep.mp4', size: mp4.length, content: mp4, ...stamp('2026-03-05T00:00:00Z') },
        { name: 'del.mp4', size: mp4.length, content: mp4, deleted: true, ...stamp('2026-03-04T00:00:00Z') },
      ],
    });
    const source = new MemorySource(built.bytes);
    const before = await hashSource(source);
    const opened = await openImage(source);
    const handle = opened.volumes[0]!;
    const scan = await scanVolume(handle, { includeDeleted: true, maxEntries: 10_000, resolveChains: true });
    for (const e of scan.entries.filter((x) => x.kind === 'file')) {
      await recoverEntry(handle.reader, e);
    }
    const after = await hashSource(source);
    expect(after).toEqual(before);
    expect(sha256Hex(built.bytes)).toBe(built.expectations.imageSha256);
  });
});
