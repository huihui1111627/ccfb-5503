/**
 * 分析编排：从字节源到卷、条目、簇链、恢复结果、雕刻命中。
 * 所有步骤只读；调用方负责在前后各计算一次镜像哈希以证明未改动。
 */
import type { ByteSource } from './source';
import type {
  CarveHit,
  FileEntry,
  PartitionInfo,
  ScanStats,
  VolumeInfo,
} from './types';
import { detectDiskLayout } from './disk';
import { FatVolume } from './fs/fat';
import { ExfatVolume } from './fs/exfat';
import { carveUnallocated } from './carve';
import { computeAllocatedRanges, computeUnallocated } from './recover';
import type { VolumeReader } from './recover';

export interface VolumeHandle {
  info: VolumeInfo;
  reader: VolumeReader;
}

export interface OpenImageResult {
  partitions: PartitionInfo[];
  volumes: VolumeHandle[];
  warnings: string[];
}

export async function openImage(
  source: ByteSource,
  signal?: AbortSignal,
): Promise<OpenImageResult> {
  const layout = await detectDiskLayout(source);
  const warnings = [...layout.warnings];
  const partitions: PartitionInfo[] = [];
  const volumes: VolumeHandle[] = [];

  for (const raw of layout.partitions) {
    const partition: PartitionInfo = {
      index: raw.index,
      scheme: raw.scheme,
      offset: raw.offset,
      size: raw.size,
      typeCode: raw.typeCode,
      typeLabel: raw.typeLabel,
      bootable: raw.bootable,
      ...(raw.name ? { name: raw.name } : {}),
      ...(raw.guid ? { guid: raw.guid } : {}),
    };
    try {
      const opened = await openVolume(source, raw.offset, raw.size, raw.index);
      if (opened) {
        partition.volume = opened.info;
        volumes.push(opened);
      }
      partitions.push(partition);
    } catch (err) {
      warnings.push(
        `分区 ${raw.index} (偏移 ${raw.offset}) 打开失败: ${err instanceof Error ? err.message : String(err)}`,
      );
      partitions.push(partition);
    }
    if (signal?.aborted) throw new DOMException('已取消', 'AbortError');
  }

  // 卷内偏移探测：分区表正确但卷起点有额外偏移的情况（部分相机）。
  for (const p of partitions) {
    if (p.volume || p.offset + 512 >= source.size) continue;
    const nested = await probeNestedVolume(source, p.offset, p.size, p.index);
    if (nested) {
      p.volume = nested.info;
      volumes.push(nested);
      warnings.push(`分区 ${p.index} 内发现嵌套卷（偏移 ${nested.info.offset}）`);
    }
  }

  return { partitions, volumes, warnings };
}

export async function openVolume(
  source: ByteSource,
  offset: number,
  size: number,
  partitionIndex: number,
): Promise<VolumeHandle | null> {
  const head = await source.read(offset, 512);
  if (String.fromCharCode(...head.subarray(3, 11)) === 'EXFAT   ') {
    const vol = await ExfatVolume.open(source, offset, size, partitionIndex);
    return { info: vol.info, reader: vol };
  }
  const { isFatBoot } = await import('./fs/fat');
  if (isFatBoot(head)) {
    const vol = await FatVolume.open(source, offset, size, partitionIndex);
    return { info: vol.info, reader: vol };
  }
  // 分区前可能有引导代码：在分区范围内找卷签名。
  return probeNestedVolume(source, offset, size, partitionIndex);
}

async function probeNestedVolume(
  source: ByteSource,
  offset: number,
  size: number,
  partitionIndex: number,
): Promise<VolumeHandle | null> {
  const limit = Math.min(offset + Math.min(size, 8 * 1024 * 1024), source.size - 512);
  for (let at = offset + 512; at <= limit; at += 512) {
    const buf = await source.read(at, 512);
    const isExfat = String.fromCharCode(...buf.subarray(3, 11)) === 'EXFAT   ';
    const fatMarker = String.fromCharCode(...buf.subarray(54, 62));
    if (isExfat) {
      const vol = await ExfatVolume.open(source, at, size - (at - offset), partitionIndex);
      return { info: vol.info, reader: vol };
    }
    if (/^FAT(12|16|32)/.test(fatMarker) && buf[510] === 0x55 && buf[511] === 0xaa) {
      try {
        const vol = await FatVolume.open(source, at, size - (at - offset), partitionIndex);
        return { info: vol.info, reader: vol };
      } catch {
        // 继续探测
      }
    }
  }
  return null;
}

export interface ScanVolumeOptions {
  includeDeleted: boolean;
  maxEntries: number;
  resolveChains: boolean;
  signal?: AbortSignal;
  onProgress?: (p: { entries: number; phase: string }) => void;
}

export interface VolumeScanResult {
  entries: FileEntry[];
  stats: ScanStats;
}

export async function scanVolume(
  handle: VolumeHandle,
  opts: ScanVolumeOptions,
): Promise<VolumeScanResult> {
  const { reader } = handle;
  const { entries, stats } = await reader.listEntries(
    {
      includeDeleted: opts.includeDeleted,
      maxEntries: opts.maxEntries,
      ...(opts.signal ? { signal: opts.signal } : {}),
    },
    () => opts.onProgress?.({ entries: 1, phase: 'listing' }),
  );

  if (opts.resolveChains) {
    // 补全簇链；逐条处理便于界面渐进展示。
    const batch = 100;
    for (let i = 0; i < entries.length; i += batch) {
      if (opts.signal?.aborted) throw new DOMException('已取消', 'AbortError');
      const slice = entries.slice(i, i + batch);
      await Promise.all(slice.map((e) => safeResolve(reader, e, opts.signal)));
      opts.onProgress?.({ entries: i + slice.length, phase: 'chains' });
    }
  }
  // 统计时排除系统元数据文件，避免 macOS 写入的 ._ 文件污染案情数字。
  const metadata = entries.filter((e) => e.isMetadata);
  if (metadata.length > 0) {
    stats.metadataFiles = metadata.length;
    stats.files -= metadata.filter((e) => !e.deleted && e.kind === 'file').length;
    if (stats.recoveredName > 0) {
      // 元数据条目也可能带长名；同步扣减，避免“恢复长文件名”数字虚高。
      stats.recoveredName -= metadata.filter((e) => e.longNameRecovered).length;
    }
    stats.deletedFiles -= metadata.filter((e) => e.deleted && e.kind === 'file').length;
    stats.directories -= metadata.filter((e) => !e.deleted && e.kind === 'directory').length;
    stats.totalBytes -= metadata
      .filter((e) => !e.deleted && e.kind === 'file')
      .reduce((sum, e) => sum + e.size, 0);
    stats.warnings.push(
      `已识别并排除 ${metadata.length} 个系统元数据条目（如 macOS 的 ._ 资源分支、.fseventsd）`,
    );
  }

  // 合并卷级警告（去重），避免同一条目录异常重复几十次。
  const seen = new Set(stats.warnings);
  for (const w of reader.info.warnings) {
    if (!seen.has(w.message)) {
      stats.warnings.push(w.message);
      seen.add(w.message);
    }
  }
  return { entries, stats };
}

async function safeResolve(
  reader: VolumeReader,
  entry: FileEntry,
  signal?: AbortSignal,
): Promise<void> {
  try {
    await reader.resolveChain(entry, signal);
  } catch (err) {
    entry.chainQuality = 'error';
    entry.warnings.push(`簇链解析失败: ${err instanceof Error ? err.message : String(err)}`);
  }
}

export interface CarveRunOptions {
  maxHits: number;
  maxObjectSize: number;
  minJpegSize: number;
  minMp4Size: number;
  signal?: AbortSignal;
  onProgress?: (p: { scannedBytes: number; totalBytes: number; hits: number }) => void;
}

export async function carveVolume(
  source: ByteSource,
  handle: VolumeHandle,
  entries: FileEntry[],
  opts: CarveRunOptions,
): Promise<{ hits: CarveHit[]; scannedBytes: number; truncated: boolean }> {
  const unallocated = computeUnallocated(handle.reader, entries);
  const allocated = computeAllocatedRanges(handle.reader, entries);
  const total = unallocated.reduce((s, r) => s + (r.end - r.start), 0);
  let lastReport = 0;
  const result = await carveUnallocated(source, handle.info.id, unallocated, {
    allocatedRanges: allocated,
    maxHits: opts.maxHits,
    maxObjectSize: opts.maxObjectSize,
    minJpegSize: opts.minJpegSize,
    minMp4Size: opts.minMp4Size,
    ...(opts.signal ? { signal: opts.signal } : {}),
    onProgress: (p) => {
      const now = Date.now();
      if (now - lastReport > 200) {
        lastReport = now;
        opts.onProgress?.({ scannedBytes: p.scannedBytes, totalBytes: total, hits: p.hits });
      }
    },
  });
  // 标记与已恢复/现存文件重叠的雕刻结果（同簇区域重复命中）
  return { hits: result.hits, scannedBytes: result.scannedBytes, truncated: result.truncated };
}
