/**
 * 文件级雕刻：按特征在未分配空间扫描 MP4 与 JPEG。
 *
 * MP4：以 box 结构（size + type）推进；遇到 `mdat` 用其长度定位结尾，
 *        再校验 `moov` 是否存在。缺 moov 的视为“不完整”。
 * JPEG：SOI 起逐段解析 marker，SOS 后扫描熵编码数据直到 EOI；
 *        无 EOI 的标记为不完整。
 *
 * 扫描按 4MiB 窗口流式读取；窗口外的对象尾部通过 ByteSource 继续读取，
 * 32GB 镜像不会整体载入内存。雕刻结果一律先去掉与已分配区域重叠的部分。
 */
import type { ByteSource } from './source';
import type { CarveHit, CarveType } from './types';
import { digestPair } from './hash';

const WINDOW = 4 * 1024 * 1024;

export interface CarveOptions {
  allocatedRanges: Array<{ start: number; end: number }>;
  maxHits: number;
  maxObjectSize: number;
  minJpegSize: number;
  minMp4Size: number;
  signal?: AbortSignal;
  onProgress?: (p: { scannedBytes: number; hits: number }) => void;
}

export interface CarveResult {
  hits: CarveHit[];
  scannedBytes: number;
  skippedAllocated: number;
  truncated: boolean;
}

interface SigHit {
  offset: number;
  type: CarveType;
}

export async function carveUnallocated(
  source: ByteSource,
  volumeId: string,
  extents: Array<{ start: number; end: number }>,
  opts: CarveOptions,
): Promise<CarveResult> {
  const hits: CarveHit[] = [];
  let scannedBytes = 0;
  let skippedAllocated = 0;
  let truncated = false;
  const seenOffsets = new Set<number>();

  for (const extent of extents) {
    let cursor = extent.start;
    while (cursor < extent.end) {
      if (opts.signal?.aborted) throw new DOMException('雕刻已取消', 'AbortError');
      if (hits.length >= opts.maxHits) {
        truncated = true;
        break;
      }
      const loadStart = Math.max(extent.start, cursor - 32);
      const windowEnd = Math.min(extent.end, cursor + WINDOW);
      const window = await source.read(loadStart, windowEnd - loadStart);
      scannedBytes += window.length;

      for (const sig of findSignatures(window, loadStart)) {
        if (sig.offset < cursor) continue; // 已在前一窗口处理
        if (seenOffsets.has(sig.offset)) continue;
        seenOffsets.add(sig.offset);
        if (opts.signal?.aborted) break;
        if (hits.length >= opts.maxHits) {
          truncated = true;
          break;
        }
        const hit = sig.type === 'jpeg'
          ? await carveJpeg(source, sig.offset, extent.end, volumeId, opts)
          : await carveMp4(source, sig.offset, extent.end, volumeId, opts);
        if (!hit) continue;
        if (overlapsAllocated(hit.offset, hit.offset + hit.size, opts.allocatedRanges)) {
          skippedAllocated += hit.size;
          continue;
        }
        hits.push(hit);
      }
      opts.onProgress?.({ scannedBytes, hits: hits.length });
      if (windowEnd >= extent.end) break;
      cursor = windowEnd;
    }
    if (truncated) break;
  }
  return { hits, scannedBytes, skippedAllocated, truncated };
}

function overlapsAllocated(
  start: number,
  end: number,
  ranges: Array<{ start: number; end: number }>,
): boolean {
  let lo = 0;
  let hi = ranges.length - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const r = ranges[mid]!;
    if (end <= r.start) hi = mid - 1;
    else if (start >= r.end) lo = mid + 1;
    else return true;
  }
  return false;
}

/** 在窗口内找 JPEG / MP4 签名；允许跨窗口重叠 3 字节由调用方处理。 */
function findSignatures(window: Uint8Array, baseOffset: number): SigHit[] {
  const hits: SigHit[] = [];
  for (let i = 0; i + 8 <= window.length; i++) {
    if (window[i] === 0xff && window[i + 1] === 0xd8 && window[i + 2] === 0xff) {
      const m = window[i + 3]!;
      if (m >= 0xc0 && m <= 0xfe && m !== 0xc4 && m !== 0xc8 && m !== 0xcc) {
        hits.push({ offset: baseOffset + i, type: 'jpeg' });
        i += 3;
        continue;
      }
    }
    // MP4：首个 box 通常为 ftyp，长度字段 8..4096，且其后紧跟 'ftyp'。
    if (
      window[i + 4] === 0x66 && window[i + 5] === 0x74 &&
      window[i + 6] === 0x79 && window[i + 7] === 0x70
    ) {
      const boxSize = readU32BE(window, i);
      if (boxSize >= 8 && boxSize <= 4096) {
        hits.push({ offset: baseOffset + i, type: 'mp4' });
        i += 7;
        continue;
      }
    }
  }
  return hits;
}

/** 读取跨越窗口的连续字节，带上限。 */
async function readSpan(
  source: ByteSource,
  start: number,
  length: number,
): Promise<Uint8Array> {
  return source.read(start, length);
}

async function carveJpeg(
  source: ByteSource,
  offset: number,
  extentEnd: number,
  volumeId: string,
  opts: CarveOptions,
): Promise<CarveHit | null> {
  const reasons: string[] = [];
  const maxLen = Math.min(opts.maxObjectSize, extentEnd - offset);
  let buf = await readSpan(source, offset, Math.min(maxLen, 64 * 1024));
  let bufferStart = offset;
  let pos = 2;
  let width = 0;
  let height = 0;
  let components = 0;
  let complete = false;
  let size = 0;
  let sosAt = -1;

  const ensure = async (needEnd: number): Promise<boolean> => {
    if (needEnd <= bufferStart + buf.length) return true;
    const want = Math.min(maxLen, needEnd - bufferStart);
    if (want <= buf.length) return false;
    buf = await readSpan(source, bufferStart, want);
    return true;
  };

  while (pos + 1 < buf.length) {
    if (buf[pos] !== 0xff) break;
    const marker = buf[pos + 1]!;
    if (marker === 0xff) { pos++; continue; }
    // pos 是相对缓冲区的下标，size 也应相对缓冲区计算（不能混入绝对偏移）。
    if (marker === 0xd9) { complete = true; size = pos + 2; break; }
    if (marker === 0xd8) { pos += 2; continue; }
    if (marker >= 0xd0 && marker <= 0xd7) { pos += 2; continue; }
    if (marker === 0x01) { pos += 2; continue; }
    if (pos + 4 > buf.length) {
      if (!(await ensure(pos + 4))) break;
      continue;
    }
    const len = readU16BE(buf, pos + 2);
    if (len < 2) { reasons.push(`段 0x${marker.toString(16)} 长度非法`); break; }

    if (marker === 0xda) {
      sosAt = pos;
      const scanEnd = Math.min(maxLen, bufferStart + buf.length - bufferStart);
      const eoi = findEoiInBuffer(buf, pos + 2 + len);
      if (eoi >= 0) {
        complete = true;
        size = eoi + 2;
        break;
      }
      // 跨缓冲区：继续向后按块读取直到 EOI 或上限。
      let scan = bufferStart + buf.length;
      const step = 1 * 1024 * 1024;
      let found = -1;
      while (scan < offset + maxLen) {
        const chunk = await source.read(scan, Math.min(step, offset + maxLen - scan));
        const e = findEoiInBuffer(chunk, 0);
        if (e >= 0) { found = scan + e; break; }
        scan += chunk.length;
      }
      if (found >= 0) {
        complete = true;
        size = found + 2 - offset;
      } else {
        reasons.push('未找到 EOI 结束标记（数据可能被截断或跨入已分配区域）');
        size = Math.min(maxLen, scan - offset);
      }
      void scanEnd;
      break;
    }

    if (marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc) {
      if (!(await ensure(pos + 10))) {
        reasons.push('SOF 段不完整');
        break;
      }
      const h = readU16BE(buf, pos + 5);
      const w = readU16BE(buf, pos + 7);
      components = buf[pos + 9] ?? 0;
      if (h) height = h;
      if (w) width = w;
    }
    pos += 2 + len;
  }

  if (size <= 0) return null;
  if (size < opts.minJpegSize) return null;
  if (size > maxLen) {
    reasons.push(`推断尺寸 ${size} 超过上限 ${maxLen}`);
    return null;
  }
  if (!complete) reasons.push('JPEG 缺少 EOI');
  const data = await readSpan(source, offset, size);
  if (data.length < 4 || data[data.length - 2] !== 0xff || data[data.length - 1] !== 0xd9) {
    if (complete) reasons.push('EOI 位置与对象末尾不一致');
    complete = false;
  }
  const hash = await digestPair(data);
  void sosAt;
  return {
    id: `carve-jpeg-${offset}`,
    volumeId,
    type: 'jpeg',
    offset,
    size,
    complete,
    reasons,
    hash,
    details: { width, height, components, segments: countSegments(data) },
    blob: new Blob([data as unknown as BlobPart], { type: 'image/jpeg' }),
    carvedAt: new Date().toISOString(),
  };
}

function findEoiInBuffer(buf: Uint8Array, from: number): number {
  for (let i = from; i + 1 < buf.length; i++) {
    if (buf[i] === 0xff) {
      const n = buf[i + 1]!;
      if (n === 0xd9) return i;
      if (n === 0x00) { i++; continue; }
    }
  }
  return -1;
}

function countSegments(data: Uint8Array): number {
  let n = 0;
  for (let i = 0; i + 3 < data.length; i++) {
    if (data[i] === 0xff && data[i + 1] !== 0x00 && data[i + 1] !== 0xff) n++;
  }
  return n;
}

async function carveMp4(
  source: ByteSource,
  offset: number,
  extentEnd: number,
  volumeId: string,
  opts: CarveOptions,
): Promise<CarveHit | null> {
  const reasons: string[] = [];
  const maxLen = Math.min(opts.maxObjectSize, extentEnd - offset);
  let buf = await readSpan(source, offset, Math.min(maxLen, 64 * 1024));
  let bufferStart = offset;
  let pos = 0; // 相对 bufferStart
  let moovSeen = false;
  let ftypSeen = false;
  let mdatSeen = false;
  let boxes = 0;
  let brands = '';
  let end = 0;

  const ensure = async (relEnd: number): Promise<boolean> => {
    const abs = bufferStart + relEnd;
    if (abs <= bufferStart + buf.length) return true;
    const want = Math.min(maxLen, abs - bufferStart);
    if (want <= buf.length || want > maxLen) return false;
    buf = await readSpan(source, bufferStart, want);
    return abs <= bufferStart + buf.length;
  };

  while (pos + 8 <= maxLen) {
    if (!(await ensure(pos + 8))) break;
    let size = readU32BE(buf, pos);
    const type = String.fromCharCode(
      buf[pos + 4]!, buf[pos + 5]!, buf[pos + 6]!, buf[pos + 7]!,
    );
    let header = 8;
    if (size === 1) {
      if (!(await ensure(pos + 16))) break;
      const hi = readU32BE(buf, pos + 8);
      const lo = readU32BE(buf, pos + 12);
      if (hi >= 0x20_0000) { reasons.push('64 位 box 长度异常'); break; }
      size = hi * 0x1_0000_0000 + lo;
      header = 16;
    } else if (size === 0) {
      reasons.push(`box ${sanitize(type)} 声明 size=0，无法界定对象边界`);
      break;
    }
    if (size < header || size > opts.maxObjectSize || size > maxLen) {
      reasons.push(`box ${sanitize(type)} 长度异常: ${size}`);
      break;
    }
    if (type === 'ftyp') {
      ftypSeen = true;
      if (pos + 12 <= buf.length) {
        brands = String.fromCharCode(
          buf[pos + 8]!, buf[pos + 9]!, buf[pos + 10]!, buf[pos + 11]!,
        ).replace(/[^\x20-\x7e]/g, '');
      }
    }
    if (type === 'moov') moovSeen = true;
    if (type === 'mdat') mdatSeen = true;
    boxes++;
    pos += size;
    end = pos;
    if (ftypSeen && moovSeen && mdatSeen) break;
  }

  if (end <= 0 || end < opts.minMp4Size) return null;
  const size = end;
  if (!ftypSeen) reasons.push('缺少 ftyp box');
  if (!moovSeen) reasons.push('缺少 moov box（文件可能被截断）');
  if (!mdatSeen) reasons.push('缺少 mdat box');
  const data = await readSpan(source, offset, size);
  const complete = ftypSeen && moovSeen && mdatSeen && data.length === size;
  if (data.length !== size) reasons.push('对象尾部超出可读范围');
  const hash = await digestPair(data);
  return {
    id: `carve-mp4-${offset}`,
    volumeId,
    type: 'mp4',
    offset,
    size,
    complete,
    reasons,
    hash,
    details: { brands, boxes, hasMoov: moovSeen ? 1 : 0 },
    blob: new Blob([data as unknown as BlobPart], { type: 'video/mp4' }),
    carvedAt: new Date().toISOString(),
  };
}

function sanitize(type: string): string {
  return type.replace(/[^0-9a-zA-Z ]/g, '?');
}

function readU16BE(buf: Uint8Array, off: number): number {
  return ((buf[off] ?? 0) << 8) | (buf[off + 1] ?? 0);
}

function readU32BE(buf: Uint8Array, off: number): number {
  return (
    (((buf[off] ?? 0) << 24) | ((buf[off + 1] ?? 0) << 16) |
      ((buf[off + 2] ?? 0) << 8) | (buf[off + 3] ?? 0)) >>> 0
  );
}

export { findSignatures, readU32BE };
