/**
 * 只读字节源抽象。所有取证读取都经由此接口，杜绝任何写回镜像的可能。
 */
export interface ByteSource {
  readonly size: number;
  read(offset: number, length: number): Promise<Uint8Array>;
}

/** 基于 File / Blob 的随机读取（大镜像不会整体载入内存）。 */
export class BlobSource implements ByteSource {
  constructor(private readonly blob: Blob) {}

  get size(): number {
    return this.blob.size;
  }

  async read(offset: number, length: number): Promise<Uint8Array> {
    validateRange(offset, length, this.blob.size);
    if (length === 0) return new Uint8Array(0);
    const buf = await this.blob.slice(offset, offset + length).arrayBuffer();
    return new Uint8Array(buf);
  }
}

/** 内存字节源，主要用于测试与小缓冲区。 */
export class MemorySource implements ByteSource {
  constructor(private readonly data: Uint8Array) {}

  get size(): number {
    return this.data.length;
  }

  async read(offset: number, length: number): Promise<Uint8Array> {
    validateRange(offset, length, this.data.length);
    return this.data.subarray(offset, offset + length);
  }
}

function validateRange(offset: number, length: number, size: number): void {
  if (!Number.isInteger(offset) || !Number.isInteger(length) || offset < 0 || length < 0) {
    throw new RangeError(`非法读取范围 offset=${offset} length=${length}`);
  }
  if (offset + length > size) {
    throw new RangeError(
      `读取越界: offset=${offset} length=${length} size=${size}`,
    );
  }
}

/**
 * 块缓存读取器：取证工具会对 FAT、目录区做大量小读取。
 * 采用固定大小块 + LRU，限制内存占用（默认 64MiB），32GB 卡也能流畅浏览。
 */
export class CachedSource implements ByteSource {
  private readonly blocks = new Map<number, Uint8Array>();
  private readonly inflight = new Map<number, Promise<Uint8Array>>();

  constructor(
    private readonly inner: ByteSource,
    private readonly blockSize = 1024 * 1024,
    private readonly maxBlocks = 64,
  ) {}

  get size(): number {
    return this.inner.size;
  }

  async read(offset: number, length: number): Promise<Uint8Array> {
    validateRange(offset, length, this.inner.size);
    if (length === 0) return new Uint8Array(0);
    const start = Math.floor(offset / this.blockSize);
    const end = Math.floor((offset + length - 1) / this.blockSize);
    if (end - start > 8) {
      // 大范围读取（如哈希、雕刻）不走缓存，避免污染目录缓存。
      return this.inner.read(offset, length);
    }
    const parts: Uint8Array[] = [];
    for (let i = start; i <= end; i++) {
      const block = await this.block(i);
      const blockStart = i * this.blockSize;
      const from = Math.max(offset, blockStart) - blockStart;
      const to = Math.min(offset + length, blockStart + block.length) - blockStart;
      parts.push(block.subarray(from, to));
    }
    return concat(parts);
  }

  /** 释放缓存，长任务间隙调用。 */
  clear(): void {
    this.blocks.clear();
  }

  private async block(index: number): Promise<Uint8Array> {
    const cached = this.blocks.get(index);
    if (cached) {
      this.blocks.delete(index);
      this.blocks.set(index, cached);
      return cached;
    }
    let pending = this.inflight.get(index);
    if (!pending) {
      const offset = index * this.blockSize;
      const length = Math.min(this.blockSize, this.inner.size - offset);
      pending = this.inner.read(offset, length).then((data) => {
        this.inflight.delete(index);
        this.blocks.set(index, data);
        if (this.blocks.size > this.maxBlocks) {
          const oldest = this.blocks.keys().next();
          if (!oldest.done) this.blocks.delete(oldest.value);
        }
        return data;
      }, (err) => {
        this.inflight.delete(index);
        throw err;
      });
      this.inflight.set(index, pending);
    }
    return pending;
  }
}

export function concat(parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((sum, p) => sum + p.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}
