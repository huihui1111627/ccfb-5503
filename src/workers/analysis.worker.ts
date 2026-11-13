/**
 * 分析 Worker：把重 IO（哈希、扫描、恢复、雕刻）全部移出主线程。
 *
 * 协议：主线程 postMessage({id, kind, payload})，Worker 回 {id, kind:'progress'|'result'|'error'}。
 * 镜像通过 File 对象传递（结构化克隆，不复制底层数据）。
 */
import type { ByteSource } from '../core/source';
import { BlobSource, CachedSource } from '../core/source';
import { hashSource, type HashProgress } from '../core/hash';
import { openImage, scanVolume, carveVolume } from '../core/analyze';
import { recoverEntry } from '../core/recover';
import { buildTimeline } from '../core/timeline';
import type {
  CarveHit,
  FileEntry,
  PartitionInfo,
  RecoveryResult,
  TimelineEvent,
  VolumeInfo,
} from '../core/types';

export type WorkerRequest =
  | { id: number; kind: 'open'; payload: { file: File } }
  | { id: number; kind: 'hash'; payload: { file: File; verifyAgainst?: { md5: string; sha256: string } } }
  | { id: number; kind: 'scan'; payload: { file: File; offset: number; size: number; partitionIndex: number; includeDeleted: boolean; maxEntries: number; fatTimezoneOffsetMinutes: number } }
  | { id: number; kind: 'recover'; payload: { file: File; offset: number; size: number; partitionIndex: number; entry: FileEntry } }
  | { id: number; kind: 'carve'; payload: { file: File; offset: number; size: number; partitionIndex: number; entries: FileEntry[]; maxHits: number; maxObjectSize: number } }
  | { id: number; kind: 'timeline'; payload: { entries: FileEntry[]; fatTimezoneOffsetMinutes: number } }
  | { id: number; kind: 'cancel'; payload: { target: number } };

export type WorkerResponse =
  | { id: number; kind: 'progress'; payload: ProgressPayload }
  | { id: number; kind: 'result'; payload: unknown }
  | { id: number; kind: 'error'; payload: { message: string; stack?: string } };

export interface ProgressPayload {
  phase: string;
  current?: number;
  total?: number;
  message?: string;
}

const controllers = new Map<number, AbortController>();
const cacheByFile = new WeakMap<File, CachedSource>();

function sourceFor(file: File): ByteSource {
  let cached = cacheByFile.get(file);
  if (!cached) {
    cached = new CachedSource(new BlobSource(file), 1024 * 1024, 48);
    cacheByFile.set(file, cached);
  }
  return cached;
}

function post(msg: WorkerResponse): void {
  (self as unknown as { postMessage: (m: unknown) => void }).postMessage(msg);
}

self.onmessage = async (event: MessageEvent<WorkerRequest>) => {
  const req = event.data;
  if (req.kind === 'cancel') {
    controllers.get(req.payload.target)?.abort();
    return;
  }
  const controller = new AbortController();
  controllers.set(req.id, controller);
  try {
    const result = await handle(req, controller);
    post({ id: req.id, kind: 'result', payload: result });
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    const stack = err instanceof Error ? err.stack : undefined;
    post({ id: req.id, kind: 'error', payload: stack ? { message, stack } : { message } });
  } finally {
    controllers.delete(req.id);
  }
};

async function handle(req: WorkerRequest, controller: AbortController): Promise<unknown> {
  const signal = controller.signal;
  switch (req.kind) {
    case 'open': {
      const source = sourceFor(req.payload.file);
      postProgress(req.id, { phase: '识别分区与卷', current: 0, total: 100 });
      const opened = await openImage(source, signal);
      return {
        partitions: opened.partitions,
        volumes: opened.volumes.map((v) => v.info),
        warnings: opened.warnings,
      } satisfies OpenResult;
    }
    case 'hash': {
      const source = sourceFor(req.payload.file);
      const digest = await hashSource(source, (p: HashProgress) => {
        postProgress(req.id, {
          phase: '计算镜像哈希',
          current: p.bytesHashed,
          total: p.totalBytes,
        });
      }, signal);
      let match: boolean | null = null;
      if (req.payload.verifyAgainst) {
        match =
          digest.md5 === req.payload.verifyAgainst.md5 &&
          digest.sha256 === req.payload.verifyAgainst.sha256;
      }
      return { ...digest, match } satisfies HashResult;
    }
    case 'scan': {
      const source = sourceFor(req.payload.file);
      const opened = await openImage(source, signal);
      const info = opened.volumes.find(
        (v) => v.info.offset === req.payload.offset,
      ) ?? opened.volumes.find((v) => v.info.partitionIndex === req.payload.partitionIndex);
      if (!info) throw new Error('未找到指定卷，镜像结构可能已变化');
      const scan = await scanVolume(info, {
        includeDeleted: req.payload.includeDeleted,
        maxEntries: req.payload.maxEntries,
        resolveChains: true,
        signal,
        onProgress: (p) => postProgress(req.id, { phase: `扫描目录项 (${p.phase})`, current: p.entries, total: req.payload.maxEntries }),
      });
      return {
        volume: info.info,
        entries: scan.entries,
        stats: scan.stats,
      } satisfies ScanResult;
    }
    case 'recover': {
      const source = sourceFor(req.payload.file);
      const opened = await openImage(source, signal);
      const info = opened.volumes.find((v) => v.info.offset === req.payload.offset);
      if (!info) throw new Error('未找到指定卷');
      // 只传单条 entry，避免把整张目录表（可能数十万条）复制进 Worker。
      const result = await recoverEntry(info.reader, req.payload.entry, { signal });
      return serializeRecovery(result);
    }
    case 'carve': {
      const source = sourceFor(req.payload.file);
      const opened = await openImage(source, signal);
      const info = opened.volumes.find((v) => v.info.offset === req.payload.offset);
      if (!info) throw new Error('未找到指定卷');
      const result = await carveVolume(source, info, req.payload.entries, {
        maxHits: req.payload.maxHits,
        maxObjectSize: req.payload.maxObjectSize,
        minJpegSize: 256,
        minMp4Size: 1024,
        signal,
        onProgress: (p) => postProgress(req.id, {
          phase: '雕刻未分配空间',
          current: p.scannedBytes,
          total: p.totalBytes,
          message: `已命中 ${p.hits} 个对象`,
        }),
      });
      return {
        hits: result.hits.map(serializeCarveHit),
        scannedBytes: result.scannedBytes,
        truncated: result.truncated,
      } satisfies CarveResultMsg;
    }
    case 'timeline': {
      const result = buildTimeline(req.payload.entries, {
        fatTimezoneOffsetMinutes: req.payload.fatTimezoneOffsetMinutes,
      });
      return result;
    }
  }
}

function postProgress(id: number, payload: ProgressPayload): void {
  post({ id, kind: 'progress', payload });
}

export interface OpenResult {
  partitions: PartitionInfo[];
  volumes: VolumeInfo[];
  warnings: string[];
}
export interface HashResult {
  md5: string;
  sha256: string;
  match: boolean | null;
}
export interface ScanResult {
  volume: VolumeInfo;
  entries: FileEntry[];
  stats: import('../core/types').ScanStats;
}
export interface SerializedRecovery extends Omit<RecoveryResult, 'blob'> {
  blobUrl: string | null;
}
export interface SerializedCarveHit extends Omit<CarveHit, 'blob'> {
  blobUrl: string | null;
}
export interface CarveResultMsg {
  hits: SerializedCarveHit[];
  scannedBytes: number;
  truncated: boolean;
}

function serializeRecovery(result: RecoveryResult): SerializedRecovery {
  const url = result.blob ? URL.createObjectURL(result.blob) : null;
  const { blob: _blob, ...rest } = result;
  void _blob;
  return { ...rest, blobUrl: url };
}

function serializeCarveHit(hit: CarveHit): SerializedCarveHit {
  const url = hit.blob ? URL.createObjectURL(hit.blob) : null;
  const { blob: _blob, ...rest } = hit;
  void _blob;
  return { ...rest, blobUrl: url };
}

export type TimelineResultMsg = {
  events: TimelineEvent[];
  invalidTimestamps: number;
  skippedEntries: number;
  bounds: { earliest: string | null; latest: string | null };
};
