/**
 * Worker 客户端封装：请求 id、超时、取消、进度回调。
 */
import type { WorkerRequest, WorkerResponse, ProgressPayload } from '../workers/analysis.worker';

export interface ProgressHandler {
  (p: ProgressPayload): void;
}

interface Pending {
  resolve: (value: unknown) => void;
  reject: (err: Error) => void;
  onProgress?: ProgressHandler;
  /** 请求对应的 worker 侧 id（用于取消）。 */
  id: number;
}

export class AnalysisClient {
  private worker: Worker;
  private nextId = 1;
  private readonly pending = new Map<number, Pending>();

  constructor() {
    this.worker = new Worker(new URL('../workers/analysis.worker.ts', import.meta.url), {
      type: 'module',
    });
    this.worker.onmessage = (event: MessageEvent<WorkerResponse>) => {
      const msg = event.data;
      const entry = this.pending.get(msg.id);
      if (!entry) return;
      if (msg.kind === 'progress') {
        entry.onProgress?.(msg.payload);
        return;
      }
      this.pending.delete(msg.id);
      if (msg.kind === 'result') entry.resolve(msg.payload);
      else entry.reject(new Error(msg.payload.message));
    };
    this.worker.onerror = (event) => {
      for (const [id, p] of this.pending) {
        p.reject(new Error(`分析线程错误: ${event.message}`));
        this.pending.delete(id);
      }
    };
  }

  /** 取消某个进行中的请求。 */
  cancelRequest(id: number): void {
    this.worker.postMessage({ id: this.nextId++, kind: 'cancel', payload: { target: id } });
  }

  request<T>(req: Omit<WorkerRequest, 'id'>, onProgress?: ProgressHandler): Promise<T> {
    const id = this.nextId++;
    const message = { ...req, id } as WorkerRequest;
    return new Promise<T>((resolve, reject) => {
      this.pending.set(id, {
        resolve: resolve as (v: unknown) => void,
        reject,
        id,
        ...(onProgress ? { onProgress } : {}),
      });
      this.worker.postMessage(message);
    });
  }

  terminate(): void {
    this.worker.terminate();
    for (const p of this.pending.values()) p.reject(new Error('分析线程已终止'));
    this.pending.clear();
  }
}
