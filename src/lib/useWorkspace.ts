import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AnalysisClient } from './analysisClient';
import { store } from './store';
import { custody } from './custodyService';
import type {
  AnalysisRecord,
  CaseRecord,
  CarveHit,
  CustodyEvent,
  FileEntry,
  ImageRecord,
  PartitionInfo,
  RecoveryResult,
  TimelineEvent,
  VolumeInfo,
} from '../core/types';
import type {
  CarveResultMsg,
  HashResult,
  OpenResult,
  ScanResult,
  SerializedRecovery,
  TimelineResultMsg,
} from '../workers/analysis.worker';
import type { ProgressPayload } from '../workers/analysis.worker';

/** 界面中的恢复结果：核心结果 + 可下载 URL（不入 IndexedDB）。 */
export interface UiRecovery extends RecoveryResult {
  blobUrl: string | null;
}
export interface UiCarveHit extends CarveHit {
  blobUrl: string | null;
}

export interface WorkspaceState {
  caseRecord: CaseRecord | null;
  images: ImageRecord[];
  activeImage: ImageRecord | null;
  analysis: {
    id: string | null;
    partitions: PartitionInfo[];
    volumes: VolumeInfo[];
    warnings: string[];
  } | null;
  entries: FileEntry[];
  stats: AnalysisRecord['stats'] | null;
  recoveries: UiRecovery[];
  carveHits: UiCarveHit[];
  timeline: TimelineResultMsg | null;
  progress: ProgressPayload | null;
  busy: string | null;
  error: string | null;
  /** 恢复历史分析时的提示（镜像本体不在库中）。 */
  restoredNotice: string | null;
  custodyEvents: CustodyEvent[];
  custodyValid: boolean | null;
}

const LAST_CASE_KEY = 'sd-forensics:last-case';

const EMPTY: WorkspaceState = {
  caseRecord: null,
  images: [],
  activeImage: null,
  analysis: null,
  entries: [],
  stats: null,
  recoveries: [],
  carveHits: [],
  timeline: null,
  progress: null,
  busy: null,
  error: null,
  restoredNotice: null,
  custodyEvents: [],
  custodyValid: null,
};

export interface WorkspaceApi {
  state: WorkspaceState;
  createCase(input: {
    caseNumber: string;
    title: string;
    examiner: string;
    organization: string;
    notes: string;
    timezoneOffsetMinutes: number;
  }): Promise<CaseRecord>;
  openCase(id: string): Promise<CaseRecord>;
  setCaseMeta(changes: Partial<CaseRecord>): Promise<void>;
  importImage(file: File): Promise<void>;
  scan(opts: { volume: VolumeInfo; includeDeleted: boolean; maxEntries: number }): Promise<ScanResult>;
  recover(entry: FileEntry): Promise<{ recovery: UiRecovery }>;
  carve(opts: { maxHits: number; maxObjectSize: number }): Promise<CarveResultMsg>;
  buildTimeline(): Promise<TimelineResultMsg | null>;
  verifyIntegrity(): Promise<{ ok: boolean; hash: HashResult }>;
  setActiveVolume(volume: VolumeInfo): void;
  clearError(): void;
  /** 为历史分析重新选择镜像文件（不重新哈希，仅恢复操作能力）。 */
  reattachImage(file: File): void;
  timelineEvents(): TimelineEvent[];
  refreshCustody(): Promise<void>;
}

export function useWorkspace(): WorkspaceApi {
  const [state, setState] = useState<WorkspaceState>(EMPTY);
  const clientRef = useRef<AnalysisClient | null>(null);
  const fileRef = useRef<File | null>(null);
  const stateRef = useRef(state);
  stateRef.current = state;

  const getClient = useCallback((): AnalysisClient => {
    if (!clientRef.current) clientRef.current = new AnalysisClient();
    return clientRef.current;
  }, []);

  useEffect(() => () => clientRef.current?.terminate(), []);

  const patch = useCallback((p: Partial<WorkspaceState>) => {
    setState((s) => ({ ...s, ...p }));
  }, []);

  const reloadCustody = useCallback(async (c: CaseRecord | null = stateRef.current.caseRecord) => {
    if (!c) return;
    const events = await custody.load(c);
    const v = await custody.verify();
    patch({ custodyEvents: [...events], custodyValid: v.valid });
  }, [patch]);

  // 刷新后自动恢复上次在办案件（仅元数据与分析结果；镜像本体需重新选择）。
  useEffect(() => {
    const lastId = localStorage.getItem(LAST_CASE_KEY);
    if (!lastId) return;
    let cancelled = false;
    (async () => {
      try {
        const record = await store.getCase(lastId);
        if (!record || cancelled) return;
        const images = await store.listImages(lastId);
        const image = images.sort((a, b) => b.importedAt.localeCompare(a.importedAt))[0];
        let restored: Partial<WorkspaceState> = { caseRecord: record, images };
        if (image) {
          const analyses = await store.listAnalyses(image.id);
          const latest = analyses.sort((a, b) => b.createdAt.localeCompare(a.createdAt))[0];
          if (latest) {
            const savedRecoveries = await store.listRecoveries(latest.id);
            restored = {
              ...restored,
              activeImage: image,
              analysis: { id: latest.id, partitions: image.partitions, volumes: latest.volumes, warnings: [] },
              entries: latest.entries,
              stats: latest.stats,
              recoveries: savedRecoveries.map((r) => ({
                entryId: r.entryId, volumeId: latest.volumes[0]?.id ?? '', name: r.name, path: r.path,
                deleted: true, method: 'fat-chain', status: r.status as RecoveryResult['status'],
                requestedSize: r.requestedSize, recoveredSize: r.recoveredSize, missingBytes: 0,
                overwrittenClusters: 0,
                hash: r.sha256 ? { md5: r.md5 ?? '', sha256: r.sha256 } : null,
                reasons: [], runs: [], extractedAt: r.createdAt, blobUrl: null,
              })),
              restoredNotice: '已载入上次分析结果。镜像文件需重新选择后才能执行新的恢复或雕刻。',
            };
          }
        }
        if (!cancelled) patch(restored);
        await reloadCustody(record);
      } catch {
        // 本地数据损坏时静默回退到无案件状态
      }
    })();
    return () => { cancelled = true; };
  }, [patch, reloadCustody]);


  const createCase = useCallback<WorkspaceApi['createCase']>(async (input) => {
    const id = `case-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const now = new Date().toISOString();
    const record: CaseRecord = { id, createdAt: now, updatedAt: now, ...input };
    await store.putCase(record);
    localStorage.setItem(LAST_CASE_KEY, record.id);
    await custody.append(record, 'case.create', record.caseNumber, {
      title: record.title,
      examiner: record.examiner,
      organization: record.organization,
    });
    patch({ caseRecord: record, images: [], activeImage: null, analysis: null, entries: [], stats: null, recoveries: [], carveHits: [], timeline: null, error: null });
    await reloadCustody(record);
    return record;
  }, [patch, reloadCustody]);

  const openCase = useCallback<WorkspaceApi['openCase']>(async (id) => {
    const record = await store.getCase(id);
    if (!record) throw new Error('案件不存在');
    localStorage.setItem(LAST_CASE_KEY, record.id);
    const images = await store.listImages(id);
    patch({
      caseRecord: record, images, activeImage: null, analysis: null,
      entries: [], stats: null, recoveries: [], carveHits: [], timeline: null, error: null,
      restoredNotice: null,
    });
    await reloadCustody(record);

    // 恢复最近一次分析结果（镜像本体需重新选择，但条目、统计与恢复摘要可复用）。
    const latestImage = images.sort((a, b) => b.importedAt.localeCompare(a.importedAt))[0];
    if (latestImage) {
      const analyses = await store.listAnalyses(latestImage.id);
      const latest = analyses.sort((a, b) => b.createdAt.localeCompare(a.createdAt))[0];
      if (latest) {
        const recoveries = await store.listRecoveries(latest.id);
        patch({
          activeImage: latestImage,
          analysis: { id: latest.id, partitions: latestImage.partitions, volumes: latest.volumes, warnings: [] },
          entries: latest.entries,
          stats: latest.stats,
          recoveries: recoveries.map((r) => ({
            entryId: r.entryId, volumeId: '', name: r.name, path: r.path, deleted: true,
            method: 'fat-chain', status: r.status as RecoveryResult['status'],
            requestedSize: r.requestedSize, recoveredSize: r.recoveredSize, missingBytes: 0,
            overwrittenClusters: 0, hash: r.sha256 ? { md5: r.md5 ?? '', sha256: r.sha256 } : null,
            reasons: [], runs: [], extractedAt: r.createdAt, blobUrl: null,
          })),
          restoredNotice: `已载入上次分析结果（${latest.createdAt.slice(0, 10)}）。镜像文件需重新选择后才能执行新的恢复/雕刻。`,
        });
      }
    }
    return record;
  }, [patch, reloadCustody]);

  const setCaseMeta = useCallback<WorkspaceApi['setCaseMeta']>(async (changes) => {
    const current = stateRef.current.caseRecord;
    if (!current) return;
    const record: CaseRecord = { ...current, ...changes, updatedAt: new Date().toISOString() };
    await store.putCase(record);
    await custody.append(record, 'case.update', record.caseNumber, { fields: Object.keys(changes) });
    patch({ caseRecord: record });
    await reloadCustody(record);
  }, [patch, reloadCustody]);

  const importImage = useCallback<WorkspaceApi['importImage']>(async (file) => {
    const c = stateRef.current.caseRecord;
    if (!c) throw new Error('请先创建或打开案件');
    const now = new Date().toISOString();
    const record: ImageRecord = {
      id: `img-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      caseId: c.id, fileName: file.name, size: file.size,
      md5: null, sha256: null, hashStatus: 'pending', importedAt: now,
      lastVerifiedAt: null, verification: 'unverified', partitions: [],
    };
    await store.putImage(record);
    await custody.append(c, 'image.import', file.name, { size: file.size });
    fileRef.current = file;
    patch({ activeImage: record, analysis: null, entries: [], stats: null, recoveries: [], carveHits: [], timeline: null, busy: '计算镜像哈希', error: null, progress: null });

    try {
      const hash = await getClient().request<HashResult>(
        { kind: 'hash', payload: { file } },
        (p) => patch({ progress: p }),
      );
      // 导入时只记录基线哈希，不声称“已复核”。只有显式执行“复核镜像哈希”
      // 并得到一致结果后，才可把 verification 置为 verified。
      const hashed: ImageRecord = {
        ...record, md5: hash.md5, sha256: hash.sha256, hashStatus: 'complete',
        lastVerifiedAt: null, verification: 'unverified',
      };
      await store.putImage(hashed);
      await custody.append(c, 'image.hash', file.name, { md5: hash.md5, sha256: hash.sha256 });
      patch({ activeImage: hashed, images: [hashed] });

      const opened = await getClient().request<OpenResult>(
        { kind: 'open', payload: { file } },
        (p) => patch({ progress: p }),
      );
      const withPartitions: ImageRecord = {
        ...hashed,
        partitions: opened.partitions.map((p) => {
          const vol = opened.volumes.find((v) => v.offset === p.offset);
          return vol ? { ...p, volume: vol } : p;
        }),
      };
      await store.putImage(withPartitions);
      await custody.append(c, 'volume.detect', file.name, {
        partitions: opened.partitions.length,
        volumes: opened.volumes.map((v) => ({ fs: v.fs, offset: v.offset, label: v.label, clusterSize: v.clusterSize })),
        warnings: opened.warnings,
      });
      patch({
        activeImage: withPartitions,
        analysis: { id: null, partitions: opened.partitions, volumes: opened.volumes, warnings: opened.warnings },
        busy: null, progress: null,
      });
      await reloadCustody(c);
    } catch (err) {
      patch({ busy: null, progress: null, error: err instanceof Error ? err.message : String(err) });
      throw err;
    }
  }, [getClient, patch, reloadCustody]);

  const scan = useCallback<WorkspaceApi['scan']>(async (opts) => {
    const c = stateRef.current.caseRecord;
    const file = fileRef.current;
    const image = stateRef.current.activeImage;
    if (!c || !file || !image) throw new Error('请先导入镜像');
    patch({ busy: '扫描目录项', progress: null, error: null });
    try {
      const result = await getClient().request<ScanResult>(
        {
          kind: 'scan',
          payload: {
            file, offset: opts.volume.offset, size: opts.volume.size,
            partitionIndex: opts.volume.partitionIndex,
            includeDeleted: opts.includeDeleted, maxEntries: opts.maxEntries,
            fatTimezoneOffsetMinutes: c.timezoneOffsetMinutes,
          },
        },
        (p) => patch({ progress: p }),
      );
      const analysisId = `an-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
      const analysis: AnalysisRecord = {
        id: analysisId,
        caseId: c.id, imageId: image.id, createdAt: new Date().toISOString(),
        volumes: stateRef.current.analysis?.volumes ?? [opts.volume],
        entries: result.entries, stats: result.stats, carveHits: [],
        integrity: {
          hashBefore: image.md5 && image.sha256 ? { md5: image.md5, sha256: image.sha256 } : null,
          hashAfter: null, identical: null, verifiedAt: null,
        },
      };
      await store.putAnalysis(analysis);
      await custody.append(c, 'file.scan', `卷 ${opts.volume.label || opts.volume.id}`, {
        files: result.stats.files, deleted: result.stats.deletedFiles,
        directories: result.stats.directories, truncated: result.stats.truncatedListing,
      });
      patch({
        entries: result.entries,
        stats: result.stats,
        busy: null,
        progress: null,
        recoveries: [],
        carveHits: [],
        timeline: null,
        analysis: stateRef.current.analysis
          ? { ...stateRef.current.analysis, id: analysisId }
          : null,
      });
      await reloadCustody(c);
      return result;
    } catch (err) {
      patch({ busy: null, progress: null, error: err instanceof Error ? err.message : String(err) });
      throw err;
    }
  }, [getClient, patch, reloadCustody]);

  const recover = useCallback<WorkspaceApi['recover']>(async (entry) => {
    const c = stateRef.current.caseRecord;
    const file = fileRef.current;
    const volume = stateRef.current.analysis?.volumes.find((v) => v.id === entry.volumeId);
    if (!c || !file || !volume) throw new Error('缺少卷或镜像上下文');
    patch({ busy: `恢复 ${entry.name}`, progress: null, error: null });
    try {
      const res = await getClient().request<SerializedRecovery>({
        kind: 'recover',
        payload: {
          file, offset: volume.offset, size: volume.size,
          partitionIndex: volume.partitionIndex,
          entry,
        },
      });
      const ui: UiRecovery = { ...res, blobUrl: res.blobUrl };
      patch({
        recoveries: [...stateRef.current.recoveries.filter((r) => r.entryId !== entry.id), ui],
        busy: null,
      });
      await store.putRecovery({
        id: `${entry.id}-${Date.now()}`,
        analysisId: stateRef.current.analysis?.id ?? 'unscoped',
        entryId: entry.id,
        name: entry.name,
        path: entry.path,
        status: res.status,
        sha256: res.hash?.sha256 ?? null,
        md5: res.hash?.md5 ?? null,
        recoveredSize: res.recoveredSize,
        requestedSize: res.requestedSize,
        createdAt: new Date().toISOString(),
      });
      await custody.append(c, 'file.recover', entry.path, {
        status: res.status, method: res.method, recoveredSize: res.recoveredSize,
        missingBytes: res.missingBytes, sha256: res.hash?.sha256 ?? null,
        overwrittenClusters: res.overwrittenClusters,
      });
      await reloadCustody(c);
      return { recovery: ui };
    } catch (err) {
      patch({ busy: null, error: err instanceof Error ? err.message : String(err) });
      throw err;
    }
  }, [getClient, patch, reloadCustody]);

  const carve = useCallback<WorkspaceApi['carve']>(async (opts) => {
    const c = stateRef.current.caseRecord;
    const file = fileRef.current;
    const volume = stateRef.current.analysis?.volumes[0];
    if (!c || !file || !volume) throw new Error('缺少卷或镜像上下文');
    patch({ busy: '雕刻未分配空间', progress: null, error: null });
    try {
      const res = await getClient().request<CarveResultMsg>(
        {
          kind: 'carve',
          payload: {
            file, offset: volume.offset, size: volume.size,
            partitionIndex: volume.partitionIndex,
            entries: stateRef.current.entries,
            maxHits: opts.maxHits, maxObjectSize: opts.maxObjectSize,
          },
        },
        (p) => patch({ progress: p }),
      );
      patch({ carveHits: res.hits as unknown as UiCarveHit[], busy: null, progress: null });
      await custody.append(c, 'carve.scan', `卷 ${volume.label || volume.id}`, {
        hits: res.hits.length, scannedBytes: res.scannedBytes, truncated: res.truncated,
      });
      await reloadCustody(c);
      return res;
    } catch (err) {
      patch({ busy: null, progress: null, error: err instanceof Error ? err.message : String(err) });
      throw err;
    }
  }, [getClient, patch, reloadCustody]);

  const buildTimeline = useCallback<WorkspaceApi['buildTimeline']>(async () => {
    const c = stateRef.current.caseRecord;
    if (!c) return null;
    patch({ busy: '构建时间线', error: null });
    const result = await getClient().request<TimelineResultMsg>({
      kind: 'timeline',
      payload: { entries: stateRef.current.entries, fatTimezoneOffsetMinutes: c.timezoneOffsetMinutes },
    });
    patch({ timeline: result, busy: null });
    await custody.append(c, 'timeline.build', '全部文件条目', {
      events: result.events.length, invalidTimestamps: result.invalidTimestamps,
    });
    await reloadCustody(c);
    return result;
  }, [getClient, patch, reloadCustody]);

  const verifyIntegrity = useCallback<WorkspaceApi['verifyIntegrity']>(async () => {
    const c = stateRef.current.caseRecord;
    const file = fileRef.current;
    const image = stateRef.current.activeImage;
    if (!c || !file || !image?.md5 || !image?.sha256) throw new Error('缺少镜像上下文');
    patch({ busy: '复核镜像哈希', progress: null, error: null });
    const hash = await getClient().request<HashResult>(
      { kind: 'hash', payload: { file, verifyAgainst: { md5: image.md5, sha256: image.sha256 } } },
      (p) => patch({ progress: p }),
    );
    const ok = hash.match === true;
    await custody.append(c, 'image.verify', image.fileName, {
      md5: hash.md5, sha256: hash.sha256, identical: ok,
    });
    const updated: ImageRecord = {
      ...image, lastVerifiedAt: new Date().toISOString(),
      verification: ok ? 'verified' : 'mismatch',
    };
    await store.putImage(updated);
    patch({ busy: null, progress: null, activeImage: updated });
    await reloadCustody(c);
    return { ok, hash };
  }, [getClient, patch, reloadCustody]);

  const setActiveVolume = useCallback((volume: VolumeInfo) => {
    const current = stateRef.current;
    if (!current.analysis) return;
    const ordered = [volume, ...current.analysis.volumes.filter((v) => v.id !== volume.id)];
    patch({ analysis: { ...current.analysis, volumes: ordered }, entries: [], stats: null, recoveries: [], carveHits: [], timeline: null });
  }, [patch]);

  const clearError = useCallback(() => patch({ error: null }), [patch]);
  const reattachImage = useCallback((file: File) => {
    fileRef.current = file;
    patch({ restoredNotice: null });
  }, [patch]);
  const timelineEvents = useCallback(() => stateRef.current.timeline?.events ?? [], []);
  const refreshCustody = useCallback(() => reloadCustody(), [reloadCustody]);

  return useMemo<WorkspaceApi>(() => ({
    state,
    createCase,
    openCase,
    setCaseMeta,
    importImage,
    scan,
    recover,
    carve,
    buildTimeline,
    verifyIntegrity,
    setActiveVolume,
    clearError,
    reattachImage,
    timelineEvents,
    refreshCustody,
  }), [
    state, createCase, openCase, setCaseMeta, importImage, scan, recover, carve,
    buildTimeline, verifyIntegrity, setActiveVolume, clearError, reattachImage, timelineEvents, refreshCustody,
  ]);
}
