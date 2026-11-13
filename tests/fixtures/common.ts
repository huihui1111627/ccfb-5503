/**
 * 测试镜像生成公共工具。所有写入均为小端，时间戳以“相机本地时间 + 时区”
 * 编码，便于验证时间线归一化。
 */
import { createHash } from 'node:crypto';

export const SECTOR = 512;

export function w8(buf: Uint8Array, off: number, v: number): void {
  buf[off] = v & 0xff;
}

export function w16(buf: Uint8Array, off: number, v: number): void {
  buf[off] = v & 0xff;
  buf[off + 1] = (v >>> 8) & 0xff;
}

export function w32(buf: Uint8Array, off: number, v: number): void {
  buf[off] = v & 0xff;
  buf[off + 1] = (v >>> 8) & 0xff;
  buf[off + 2] = (v >>> 16) & 0xff;
  buf[off + 3] = (v >>> 24) & 0xff;
}

export function w64(buf: Uint8Array, off: number, v: number): void {
  const lo = v % 0x1_0000_0000;
  const hi = Math.floor(v / 0x1_0000_0000);
  w32(buf, off, lo);
  w32(buf, off + 4, hi);
}

export function hexToBytes(hex: string): Uint8Array {
  const clean = hex.replace(/[^0-9a-f]/gi, '');
  const out = new Uint8Array(clean.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16);
  return out;
}

export function hex(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString('hex');
}

export function sha256Hex(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex');
}

export function md5Hex(data: Uint8Array): string {
  return createHash('md5').update(data).digest('hex');
}

export function asciiBytes(s: string, len: number): Uint8Array {
  const out = new Uint8Array(len).fill(0x20);
  for (let i = 0; i < len && i < s.length; i++) out[i] = s.charCodeAt(i) & 0xff;
  return out;
}

/** FAT/exFAT 的打包日期时间（本地时间字段）。 */
export interface PackedTime {
  date: number;
  time: number;
  tenths: number;
}

/**
 * 将“相机时钟显示的本地时间”编码为 FAT 时间字段。
 * 传入 UTC Date 与案件时区偏移（分钟），返回本地时间字段。
 */
export function packFatTime(utc: Date, tzOffsetMinutes: number): PackedTime {
  const local = new Date(utc.getTime() + tzOffsetMinutes * 60_000);
  return packLocalParts(
    local.getUTCFullYear(),
    local.getUTCMonth() + 1,
    local.getUTCDate(),
    local.getUTCHours(),
    local.getUTCMinutes(),
    local.getUTCSeconds(),
    local.getUTCMilliseconds(),
  );
}

export function packLocalParts(
  year: number,
  month: number,
  day: number,
  hour: number,
  minute: number,
  second: number,
  ms: number,
): PackedTime {
  const date = ((year - 1980) << 9) | (month << 5) | day;
  const time = (hour << 11) | (minute << 5) | Math.floor(second / 2);
  const tenths = Math.min(199, Math.floor((second % 2) * 100 + ms / 10));
  return { date: date & 0xffff, time: time & 0xffff, tenths };
}

/** exFAT UTC 偏移字节：15 分钟单位，-96..+103。 */
export function exfatTzByte(tzOffsetMinutes: number): number {
  const units = Math.round(tzOffsetMinutes / 15);
  return units < 0 ? 256 + units : units;
}

export function writeAt(buf: Uint8Array, offset: number, data: Uint8Array): void {
  buf.set(data, offset);
}

export function cloneBytes(data: Uint8Array): Uint8Array {
  return new Uint8Array(data);
}

export function fillRandom(buf: Uint8Array, start: number, length: number, seed: number): void {
  let x = seed >>> 0 || 1;
  for (let i = 0; i < length; i++) {
    x ^= x << 13;
    x >>>= 0;
    x ^= x >>> 17;
    x ^= x << 5;
    x >>>= 0;
    buf[start + i] = x & 0xff;
  }
}
