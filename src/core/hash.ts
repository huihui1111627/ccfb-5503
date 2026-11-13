/**
 * 哈希与完整性校验。
 *
 * 浏览器主线程使用 hash-wasm（WASM 加速）；Node 测试环境同一实现可直接运行。
 * 所有对外结果统一为小写十六进制。
 */
import { createMD5, createSHA1, createSHA256, createXXHash64 } from 'hash-wasm';
import type { ByteSource } from './source';

export interface DigestPair {
  md5: string;
  sha256: string;
}

export interface HashProgress {
  bytesHashed: number;
  totalBytes: number;
}

const CHUNK = 8 * 1024 * 1024;

interface HashHandle {
  update(data: Uint8Array): void;
  digest(): string;
}

async function makeHandle(name: 'md5' | 'sha256' | 'sha1'): Promise<HashHandle> {
  if (name === 'md5') {
    const h = await createMD5();
    return { update: (d) => h.update(d), digest: () => h.digest('hex') };
  }
  if (name === 'sha1') {
    const h = await createSHA1();
    return { update: (d) => h.update(d), digest: () => h.digest('hex') };
  }
  const h = await createSHA256();
  return { update: (d) => h.update(d), digest: () => h.digest('hex') };
}

/** 流式计算整镜像的 MD5 与 SHA-256（单次顺序读取）。 */
export async function hashSource(
  source: ByteSource,
  onProgress?: (p: HashProgress) => void,
  signal?: AbortSignal,
): Promise<DigestPair> {
  const md5h = await makeHandle('md5');
  const sha256h = await makeHandle('sha256');
  const total = source.size;
  let hashed = 0;
  for (let offset = 0; offset < total; offset += CHUNK) {
    if (signal?.aborted) throw new DOMException('哈希计算已取消', 'AbortError');
    const len = Math.min(CHUNK, total - offset);
    const data = await source.read(offset, len);
    md5h.update(data);
    sha256h.update(data);
    hashed += len;
    onProgress?.({ bytesHashed: hashed, totalBytes: total });
  }
  return { md5: md5h.digest(), sha256: sha256h.digest() };
}

/** 只计算 SHA-256，恢复文件与雕刻文件使用。 */
export async function sha256(data: Uint8Array): Promise<string> {
  const h = await makeHandle('sha256');
  h.update(data);
  return h.digest();
}

export async function md5(data: Uint8Array): Promise<string> {
  const h = await makeHandle('md5');
  h.update(data);
  return h.digest();
}

export async function sha1(data: Uint8Array): Promise<string> {
  const h = await makeHandle('sha1');
  h.update(data);
  return h.digest();
}

export async function digestPair(data: Uint8Array): Promise<DigestPair> {
  const [m, s] = await Promise.all([md5(data), sha256(data)]);
  return { md5: m, sha256: s };
}

/** 快速完整性采样（例如预检阶段）。 */
export async function xxhash64Hex(data: Uint8Array): Promise<string> {
  const h = await createXXHash64();
  h.update(data);
  return h.digest('hex');
}

/** 增量哈希器，供雕刻与恢复流式写入。 */
export async function createDigester(): Promise<{
  update(data: Uint8Array): void;
  digest(): Promise<DigestPair>;
}> {
  const m = await createMD5();
  const s = await createSHA256();
  return {
    update(data) {
      m.update(data);
      s.update(data);
    },
    async digest() {
      return { md5: m.digest('hex'), sha256: s.digest('hex') };
    },
  };
}
