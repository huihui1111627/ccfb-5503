/**
 * 持久化：案件、镜像元数据、分析结果、监管链。
 *
 * 镜像本体不复制进 IndexedDB（32GB 不现实）；保存 File System Access API 的
 * 文件句柄或提示用户重新选择文件。分析结果（条目、恢复摘要、雕刻命中）持久化，
 * 便于跨会话复核。
 */
import { openDB, type IDBPDatabase } from 'idb';
import type {
  AnalysisRecord,
  CaseRecord,
  CustodyEvent,
  ImageRecord,
} from '../core/types';

const DB_NAME = 'sd-forensics';
const DB_VERSION = 1;

export interface StoredRecovery {
  id: string;
  analysisId: string;
  entryId: string;
  name: string;
  path: string;
  status: string;
  sha256: string | null;
  md5: string | null;
  recoveredSize: number;
  requestedSize: number;
  createdAt: string;
  /** 恢复的数据（可选，避免把 32GB 数据写进浏览器存储）。 */
  data?: Uint8Array;
}

interface Schema {
  cases: CaseRecord;
  images: ImageRecord;
  analyses: AnalysisRecord;
  custody: CustodyEvent;
  recoveries: StoredRecovery;
  settings: { key: string; value: unknown };
}

let db: IDBPDatabase<Schema> | null = null;

async function getDb(): Promise<IDBPDatabase<Schema>> {
  if (db) return db;
  db = await openDB<Schema>(DB_NAME, DB_VERSION, {
    upgrade(database) {
      database.createObjectStore('cases', { keyPath: 'id' });
      database.createObjectStore('images', { keyPath: 'id' });
      const analyses = database.createObjectStore('analyses', { keyPath: 'id' });
      analyses.createIndex('byCase', 'caseId');
      analyses.createIndex('byImage', 'imageId');
      const custody = database.createObjectStore('custody', { keyPath: 'seq' });
      custody.createIndex('byCase', 'caseId');
      database.createObjectStore('recoveries', { keyPath: 'id' });
      database.createObjectStore('settings', { keyPath: 'key' });
    },
  });
  return db;
}

export const store = {
  async listCases(): Promise<CaseRecord[]> {
    const database = await getDb();
    return (await database.getAll('cases')) as CaseRecord[];
  },
  async getCase(id: string): Promise<CaseRecord | undefined> {
    const database = await getDb();
    return (await database.get('cases', id)) as CaseRecord | undefined;
  },
  async putCase(record: CaseRecord): Promise<void> {
    const database = await getDb();
    await database.put('cases', record);
  },
  async deleteCase(id: string): Promise<void> {
    const database = await getDb();
    const tx = database.transaction(['cases', 'images', 'analyses', 'custody', 'recoveries'], 'readwrite');
    await tx.objectStore('cases').delete(id);
    for (const storeName of ['images', 'analyses', 'recoveries'] as const) {
      const all = await tx.objectStore(storeName).getAll();
      for (const row of all as Array<{ caseId?: string }>) {
        if (row.caseId === id) await tx.objectStore(storeName).delete((row as { id: string }).id);
      }
    }
    const events = await tx.objectStore('custody').index('byCase').getAll(id);
    for (const e of events) await tx.objectStore('custody').delete(e.seq);
    await tx.done;
  },

  async listImages(caseId?: string): Promise<ImageRecord[]> {
    const database = await getDb();
    const all = (await database.getAll('images')) as ImageRecord[];
    return caseId ? all.filter((i) => i.caseId === caseId) : all;
  },
  async putImage(record: ImageRecord): Promise<void> {
    const database = await getDb();
    await database.put('images', record);
  },

  async listAnalyses(imageId?: string): Promise<AnalysisRecord[]> {
    const database = await getDb();
    if (!imageId) return (await database.getAll('analyses')) as AnalysisRecord[];
    return (await database.getAllFromIndex('analyses', 'byImage', imageId)) as AnalysisRecord[];
  },
  async putAnalysis(record: AnalysisRecord): Promise<void> {
    const database = await getDb();
    await database.put('analyses', record);
  },

  async listCustody(caseId: string): Promise<CustodyEvent[]> {
    const database = await getDb();
    const events = (await database.getAllFromIndex('custody', 'byCase', caseId)) as CustodyEvent[];
    return events.sort((a, b) => a.seq - b.seq);
  },
  async putCustody(events: CustodyEvent[]): Promise<void> {
    const database = await getDb();
    const tx = database.transaction('custody', 'readwrite');
    for (const e of events) await tx.store.put(e);
    await tx.done;
  },

  async putRecovery(record: StoredRecovery): Promise<void> {
    const database = await getDb();
    await database.put('recoveries', record);
  },
  async listRecoveries(analysisId: string): Promise<StoredRecovery[]> {
    const database = await getDb();
    const all = (await database.getAll('recoveries')) as StoredRecovery[];
    return all.filter((r) => r.analysisId === analysisId);
  },
  async listRecoveriesForCase(caseId: string): Promise<StoredRecovery[]> {
    const database = await getDb();
    const analyses = (await database.getAll('analyses')) as AnalysisRecord[];
    const ids = new Set(analyses.filter((a) => a.caseId === caseId).map((a) => a.id));
    const all = (await database.getAll('recoveries')) as StoredRecovery[];
    return all.filter((r) => ids.has(r.analysisId));
  },
};
