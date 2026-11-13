/**
 * 恢复编排：把 FileEntry 的簇链解析为可下载 Blob，计算哈希并给出完整性结论。
 *
 * 完整性判定遵循“宁可标不完整，也不假装完整”的原则：
 *  - 簇链完整、大小吻合、未被重新分配 → complete
 *  - 簇链断裂/连续假设但数据缺失 → partial（附缺失字节数与原因）
 *  - 全部数据簇已被现存文件占用 → overwritten
 */
import type {
  ClusterRun,
  FileEntry,
  RecoveryResult,
  RecoveryStatus,
} from './types';
import { digestPair } from './hash';
import type { FatVolume } from './fs/fat';
import type { ExfatVolume } from './fs/exfat';

export type VolumeReader = FatVolume | ExfatVolume;

export interface RecoverOptions {
  signal?: AbortSignal;
  /** 生成 Blob 提供给界面下载；测试环境可关闭以减少内存。 */
  createBlob?: boolean;
}

export async function recoverEntry(
  volume: VolumeReader,
  entry: FileEntry,
  opts: RecoverOptions = {},
): Promise<RecoveryResult> {
  if (entry.kind === 'directory') {
    throw new Error('不能把目录作为文件恢复');
  }
  // 补全簇链（对已删除文件尤其重要）
  if (entry.runs.length === 0 && entry.size > 0) {
    await volume.resolveChain(entry, opts.signal);
  }

  const method: RecoveryResult['method'] = volume.info.fs === 'exfat'
    ? (entry.chainQuality === 'contiguous-assumed' ? 'exfat-contiguous' : 'exfat-chain')
    : (entry.chainQuality === 'contiguous-assumed' ? 'contiguous' : 'fat-chain');

  const reasons: string[] = [];
  let status: RecoveryStatus;
  let recoveredSize = 0;
  let missingBytes = 0;
  let hash: RecoveryResult['hash'] = null;
  let blob: Blob | undefined;
  const runs: ClusterRun[] = [...entry.runs];

  if (entry.size === 0) {
    const empty = new Uint8Array(0);
    hash = await digestPair(empty);
    status = 'complete';
    reasons.push('零字节文件，无数据簇');
  } else if (entriesPerChainUnusable(entry)) {
    status = 'failed';
    reasons.push(...entry.warnings);
    reasons.push('起始簇无效，无法读取任何数据');
  } else {
    try {
      const { data, missingBytes: missing } = await volume.readFileData(entry, opts.signal);
      recoveredSize = data.length;
      missingBytes = missing;
      const digest = await digestPair(data);
      hash = digest;

      if (missing > 0) {
        status = 'partial';
        reasons.push(
          `簇链可读部分 ${data.length} 字节，小于文件声明大小 ${entry.size} 字节，缺失 ${missing} 字节`,
        );
      } else if ((entry.overwrittenClusters ?? 0) >= totalClusters(entry.runs) &&
                 totalClusters(entry.runs) > 0) {
        status = 'overwritten';
        reasons.push('全部数据簇已被现存文件重新分配，内容无法作为原件采信');
      } else if ((entry.overwrittenClusters ?? 0) > 0) {
        status = 'partial';
        reasons.push(
          `${entry.overwrittenClusters}/${totalClusters(entry.runs)} 个数据簇已被重新分配，内容可能被覆盖`,
        );
      } else if (entry.chainQuality === 'contiguous-assumed') {
        // 连续假设下字节可读，但簇归属只是推断；对外结论标为 partial，
        // 避免把“未验证的分配布局”当成完整原件采信。
        status = 'partial';
        reasons.push(
          '簇链已缺失，按连续簇假设恢复；数据区未被重新分配，但分配布局未经 FAT 证实，需结合其他证据复核',
        );
      } else if (entry.chainQuality === 'truncated' || entry.chainQuality === 'loop') {
        status = 'partial';
        reasons.push(`簇链状态 ${entry.chainQuality}，即使长度足够也应人工复核`);
      } else {
        status = 'complete';
      }
      reasons.push(...entry.warnings);

      if (opts.createBlob !== false && data.length > 0) {
        // 生成 Blob 以便浏览器下载；数据已经完整读入，这里不重复读盘。
        blob = new Blob([data as unknown as BlobPart], { type: guessMime(entry.name) });
      }
    } catch (err) {
      status = 'failed';
      reasons.push(`读取失败: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  return {
    entryId: entry.id,
    volumeId: entry.volumeId,
    name: entry.name,
    path: entry.path,
    deleted: entry.deleted,
    method,
    status,
    requestedSize: entry.size,
    recoveredSize,
    missingBytes,
    overwrittenClusters: entry.overwrittenClusters ?? 0,
    hash,
    reasons,
    runs,
    ...(blob ? { blob } : {}),
    extractedAt: new Date().toISOString(),
  };
}

function entriesPerChainUnusable(entry: FileEntry): boolean {
  return entry.firstCluster < 2 && entry.size > 0;
}

function totalClusters(runs: ClusterRun[]): number {
  return runs.reduce((s, r) => s + r.count, 0);
}

function guessMime(name: string): string {
  const ext = name.split('.').pop()?.toLowerCase() ?? '';
  const map: Record<string, string> = {
    mp4: 'video/mp4',
    mov: 'video/quicktime',
    jpg: 'image/jpeg',
    jpeg: 'image/jpeg',
    png: 'image/png',
    txt: 'text/plain',
  };
  return map[ext] ?? 'application/octet-stream';
}

/** 未分配区间（绝对偏移）：卷数据区减去所有现存文件/目录占用的簇。 */
export function computeUnallocated(
  volume: VolumeReader,
  entries: FileEntry[],
): Array<{ start: number; end: number }> {
  const clusterSize = volume.info.clusterSize;
  const totalClusters = volume.info.clusterCount;
  const allocated = new Uint8Array(totalClusters);
  const markRuns = (runs: ClusterRun[]): void => {
    for (const r of runs) {
      for (let i = 0; i < r.count; i++) {
        const c = r.start + i - 2;
        if (c >= 0 && c < totalClusters) allocated[c] = 1;
      }
    }
  };
  for (const e of entries) {
    if (e.deleted) continue; // 已删除条目的簇视为未分配（这正是雕刻要覆盖的区域）
    markRuns(e.runs);
    // 目录即便 size=0 也占簇
    if (e.kind === 'directory' && e.runs.length === 0 && e.firstCluster >= 2) {
      markRuns([{ start: e.firstCluster, count: 1 }]);
    }
  }

  const out: Array<{ start: number; end: number }> = [];
  let runStart = -1;
  for (let i = 0; i <= totalClusters; i++) {
    const free = i < totalClusters && allocated[i] === 0;
    if (free && runStart < 0) runStart = i;
    if (!free && runStart >= 0) {
      const start = volume.info.dataOffset + runStart * clusterSize;
      const end = volume.info.dataOffset + i * clusterSize;
      out.push({ start, end: Math.min(end, volume.info.offset + volume.info.size) });
      runStart = -1;
    }
  }
  return out;
}

/** 已分配区间（绝对偏移），雕刻时用于剔除误报。 */
export function computeAllocatedRanges(
  volume: VolumeReader,
  entries: FileEntry[],
): Array<{ start: number; end: number }> {
  const clusterSize = volume.info.clusterSize;
  const ranges: Array<{ start: number; end: number }> = [];
  for (const e of entries) {
    if (e.deleted) continue;
    const runs = e.runs.length > 0
      ? e.runs
      : e.kind === 'directory' && e.firstCluster >= 2
        ? [{ start: e.firstCluster, count: 1 }]
        : [];
    for (const r of runs) {
      const start = volume.info.dataOffset + (r.start - 2) * clusterSize;
      ranges.push({ start, end: start + r.count * clusterSize });
    }
  }
  ranges.sort((a, b) => a.start - b.start);
  // 合并
  const merged: Array<{ start: number; end: number }> = [];
  for (const r of ranges) {
    const last = merged[merged.length - 1];
    if (last && r.start <= last.end) last.end = Math.max(last.end, r.end);
    else merged.push({ ...r });
  }
  return merged;
}

export { digestPair };
