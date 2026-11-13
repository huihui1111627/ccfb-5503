/**
 * 小端/大端字节读取工具。取证代码里所有偏移都显式标注，避免隐式歧义。
 */

export class Reader {
  constructor(
    readonly data: Uint8Array,
    readonly baseOffset = 0,
  ) {}

  get length(): number {
    return this.data.length;
  }

  /** 越界检查；返回相对偏移。 */
  private check(offset: number, length: number): number {
    if (offset < 0 || length < 0 || offset + length > this.data.length) {
      throw new RangeError(
        `读取越界: offset=${offset} length=${length} buffer=${this.data.length}`,
      );
    }
    return offset;
  }

  u8(offset: number): number {
    this.check(offset, 1);
    return this.data[offset]!;
  }

  u16le(offset: number): number {
    this.check(offset, 2);
    return this.data[offset]! | (this.data[offset + 1]! << 8);
  }

  u32le(offset: number): number {
    this.check(offset, 4);
    return (
      this.data[offset]! |
      (this.data[offset + 1]! << 8) |
      (this.data[offset + 2]! << 16) |
      (this.data[offset + 3]! << 24)
    ) >>> 0;
  }

  /** 32 位有符号。 */
  i32le(offset: number): number {
    return this.u32le(offset) | 0;
  }

  /** 48 位小端，exFAT 时间戳使用。 */
  u48le(offset: number): number {
    const lo = this.u32le(offset);
    const hi = this.u16le(offset + 4);
    return hi * 0x1_0000_0000 + lo;
  }

  u64le(offset: number): number {
    const lo = this.u32le(offset);
    const hi = this.u32le(offset + 4);
    const value = hi * 0x1_0000_0000 + lo;
    if (!Number.isSafeInteger(value)) {
      throw new RangeError(`64 位值超出安全整数范围: ${offset}`);
    }
    return value;
  }

  bytes(offset: number, length: number): Uint8Array {
    this.check(offset, length);
    return this.data.subarray(offset, offset + length);
  }

  ascii(offset: number, length: number): string {
    let out = '';
    const end = Math.min(offset + length, this.data.length);
    for (let i = offset; i < end; i++) {
      const b = this.data[i]!;
      if (b === 0) break;
      out += String.fromCharCode(b);
    }
    return out;
  }

  /** 绝对镜像偏移，用于日志与报告定位。 */
  abs(offset: number): number {
    return this.baseOffset + offset;
  }
}

export function hex(data: Uint8Array, max = 64): string {
  const n = Math.min(data.length, max);
  let out = '';
  for (let i = 0; i < n; i++) out += data[i]!.toString(16).padStart(2, '0');
  return out + (data.length > max ? '…' : '');
}

/** 写入小端 16 位，测试镜像生成器使用。 */
export function writeU16LE(buf: Uint8Array, offset: number, value: number): void {
  buf[offset] = value & 0xff;
  buf[offset + 1] = (value >>> 8) & 0xff;
}

export function writeU32LE(buf: Uint8Array, offset: number, value: number): void {
  buf[offset] = value & 0xff;
  buf[offset + 1] = (value >>> 8) & 0xff;
  buf[offset + 2] = (value >>> 16) & 0xff;
  buf[offset + 3] = (value >>> 24) & 0xff;
}

export function writeU64LE(buf: Uint8Array, offset: number, value: number): void {
  const lo = value % 0x1_0000_0000;
  const hi = Math.floor(value / 0x1_0000_0000);
  writeU32LE(buf, offset, lo);
  writeU32LE(buf, offset + 4, hi);
}
