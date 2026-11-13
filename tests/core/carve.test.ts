/**
 * 未分配空间雕刻测试：JPEG / MP4 已知答案、完整性判定、已分配区排除。
 */
import { describe, expect, it } from 'vitest';
import { MemorySource } from '../../src/core/source';
import { openImage, scanVolume, carveVolume } from '../../src/core/analyze';
import { carveUnallocated } from '../../src/core/carve';
import { buildFat32 } from '../fixtures/fat32-builder';
import { makeMp4, tinyJpeg } from '../fixtures/media';

const t = (s: string): Date => new Date(s);
const stamp = { created: t('2026-01-01T00:00:00Z'), modified: t('2026-01-01T00:00:00Z'), accessed: t('2026-01-01T00:00:00Z') };

function carveImage(contents: Array<{ name: string; data: Uint8Array; deleted: boolean }>) {
  return buildFat32({
    sizeBytes: 64 * 1024 * 1024,
    sectorsPerCluster: 8,
    files: contents.map((c) => ({
      shortName: c.name,
      name: c.name,
      size: c.data.length,
      content: c.data,
      deleted: c.deleted,
      clearFatOnDelete: true,
      ...stamp,
    })),
  });
}

describe('JPEG 雕刻', () => {
  it('从已删除文件的数据区雕刻出完整 JPEG，尺寸与哈希匹配', async () => {
    const jpeg = tinyJpeg();
    const built = carveImage([{ name: 'IMG_0001.JPG', data: jpeg, deleted: true }]);
    const source = new MemorySource(built.bytes);
    const image = await openImage(source);
    const handle = image.volumes[0]!;
    const scan = await scanVolume(handle, { includeDeleted: true, maxEntries: 10_000, resolveChains: true });
    const result = await carveVolume(source, handle, scan.entries, {
      maxHits: 20, maxObjectSize: 8 * 1024 * 1024, minJpegSize: 64, minMp4Size: 64,
    });
    const hits = result.hits.filter((h) => h.type === 'jpeg');
    expect(hits.length).toBe(1);
    expect(hits[0]!.size).toBe(jpeg.length);
    expect(hits[0]!.complete).toBe(true);
    const digest = await import('../../src/core/hash');
    expect(hits[0]!.hash.sha256).toBe(await digest.sha256(jpeg));
  });

  it('截断的 JPEG（无 EOI）被标记为不完整，不伪造大小', async () => {
    const jpeg = tinyJpeg();
    const truncated = jpeg.subarray(0, jpeg.length - 40);
    const buf = new Uint8Array(8192);
    buf.set(truncated, 16);
    const source = new MemorySource(buf);
    const result = await carveUnallocated(source, 'v', [{ start: 0, end: buf.length }], {
      allocatedRanges: [], maxHits: 10, maxObjectSize: 1 << 20, minJpegSize: 16, minMp4Size: 16,
    });
    expect(result.hits.length).toBe(1);
    expect(result.hits[0]!.complete).toBe(false);
    expect(result.hits[0]!.reasons.join(' ')).toMatch(/EOI/);
  });
});

describe('MP4 雕刻', () => {
  it('从已删除录像的数据区雕刻出完整 MP4（ftyp+mdat+moov）', async () => {
    const mp4 = makeMp4(2);
    expect(mp4, '需要系统 ffmpeg 生成测试 MP4').not.toBeNull();
    const built = carveImage([{ name: 'REC_0001.MP4', data: mp4!, deleted: true }]);
    const source = new MemorySource(built.bytes);
    const image = await openImage(source);
    const handle = image.volumes[0]!;
    const scan = await scanVolume(handle, { includeDeleted: true, maxEntries: 10_000, resolveChains: true });
    const result = await carveVolume(source, handle, scan.entries, {
      maxHits: 20, maxObjectSize: 16 * 1024 * 1024, minJpegSize: 64, minMp4Size: 64,
    });
    const hits = result.hits.filter((h) => h.type === 'mp4');
    expect(hits.length).toBe(1);
    expect(hits[0]!.size).toBe(mp4!.length);
    expect(hits[0]!.complete).toBe(true);
    expect(hits[0]!.details.brands).toContain('isom');
  });

  it('缺少 moov 的 MP4 被标记不完整，并说明缺少的 box', async () => {
    const mp4 = makeMp4(2);
    if (!mp4) return;
    // 找到 moov 并截掉它
    let pos = 0;
    let cut = mp4.length;
    while (pos + 8 <= mp4.length) {
      const size = ((mp4[pos]! << 24) | (mp4[pos + 1]! << 16) | (mp4[pos + 2]! << 8) | mp4[pos + 3]!) >>> 0;
      const type = String.fromCharCode(mp4[pos + 4]!, mp4[pos + 5]!, mp4[pos + 6]!, mp4[pos + 7]!);
      if (type === 'moov') cut = pos;
      if (size < 8) break;
      pos += size;
    }
    const buf = new Uint8Array(65536);
    buf.set(mp4.subarray(0, cut), 0);
    const source = new MemorySource(buf);
    const result = await carveUnallocated(source, 'v', [{ start: 0, end: buf.length }], {
      allocatedRanges: [], maxHits: 10, maxObjectSize: 1 << 20, minJpegSize: 16, minMp4Size: 16,
    });
    const hit = result.hits.find((h) => h.type === 'mp4');
    expect(hit).toBeDefined();
    expect(hit!.complete).toBe(false);
    expect(hit!.reasons.join(' ')).toMatch(/moov/);
    expect(hit!.size).toBe(cut);
  });

  it('随机噪声不产生误报', async () => {
    const noise = new Uint8Array(1 * 1024 * 1024);
    let x = 0x12345678;
    for (let i = 0; i < noise.length; i++) {
      x ^= x << 13; x >>>= 0; x ^= x >>> 17; x ^= x << 5; x >>>= 0;
      noise[i] = x & 0xff;
    }
    const source = new MemorySource(noise);
    const result = await carveUnallocated(source, 'v', [{ start: 0, end: noise.length }], {
      allocatedRanges: [], maxHits: 100, maxObjectSize: 1 << 20, minJpegSize: 64, minMp4Size: 64,
    });
    expect(result.hits.length).toBe(0);
  });
});

describe('雕刻边界', () => {
  it('不雕刻现存文件占用的簇', async () => {
    const jpeg = tinyJpeg();
    const built = carveImage([{ name: 'IMG_KEEP.JPG', data: jpeg, deleted: false }]);
    const source = new MemorySource(built.bytes);
    const image = await openImage(source);
    const handle = image.volumes[0]!;
    const scan = await scanVolume(handle, { includeDeleted: true, maxEntries: 10_000, resolveChains: true });
    const result = await carveVolume(source, handle, scan.entries, {
      maxHits: 20, maxObjectSize: 8 * 1024 * 1024, minJpegSize: 64, minMp4Size: 64,
    });
    expect(result.hits.filter((h) => h.type === 'jpeg')).toHaveLength(0);
  });
});
