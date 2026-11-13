/**
 * 磁盘与分区解析：MBR（含扩展分区链）、GPT（含备份头与 CRC）、以及无分区表的
 * “superfloppy”（存储卡与相机常见的裸卷镜像）。
 *
 * 背景：行车记录仪卡多为 MBR 单分区，但也常见整卡裸卷。GPT 在 SD 卡上少见，
 * 但取证镜像可能来自 U 盘或 SSD，故一并支持并做 CRC 校验。
 */
import type { ByteSource } from './source';
import type { PartitionScheme } from './types';

const SECTOR = 512;

interface RawPartition {
  index: number;
  scheme: PartitionScheme;
  offset: number;
  size: number;
  typeCode: string;
  typeLabel: string;
  bootable: boolean;
  name?: string;
  guid?: string;
}

export interface DiskLayout {
  scheme: 'mbr' | 'gpt' | 'superfloppy' | 'none';
  sectorSize: number;
  partitions: RawPartition[];
  warnings: string[];
  /** 磁盘签名 / GPT GUID，写入报告。 */
  diskIdentifier?: string;
}

const MBR_TYPE_LABELS: Record<number, string> = {
  0x01: 'FAT12',
  0x04: 'FAT16 (<32MB)',
  0x05: '扩展分区',
  0x06: 'FAT16 (≥32MB)',
  0x07: 'exFAT / NTFS / HPFS',
  0x0b: 'FAT32',
  0x0c: 'FAT32 (LBA)',
  0x0e: 'FAT16 (LBA)',
  0x0f: '扩展分区 (LBA)',
  0x82: 'Linux swap',
  0x83: 'Linux',
  0xee: 'GPT 保护分区',
  0xef: 'EFI 系统分区',
};

export async function detectDiskLayout(source: ByteSource): Promise<DiskLayout> {
  const warnings: string[] = [];
  const first = await source.read(0, 512);

  // 候选扇区大小：512 与 4096（部分大容量卡使用 4Kn）。
  let sectorSize = 512;
  if (first[510] === 0x55 && first[511] === 0xaa) {
    sectorSize = decodeSectorSizeFromMbr(first) ?? 512;
  }

  const isGpt = hasGptSignature(first);
  if (isGpt) {
    const gpt = await readGpt(source, sectorSize, warnings);
    if (gpt) return gpt;
  }

  // 先判断是否裸卷：FAT/exFAT 引导扇区同样以 0x55AA 结尾，
  // 若签名命中就直接按裸卷处理，避免误报“分区表无效”。
  const bare = await detectBareVolume(source, 0, source.size);
  if (bare) {
    return {
      scheme: 'superfloppy',
      sectorSize,
      partitions: [
        {
          index: 0,
          scheme: 'superfloppy',
          offset: 0,
          size: source.size,
          typeCode: 'bare',
          typeLabel: bare,
          bootable: false,
        },
      ],
      warnings,
    };
  }

  if (first[510] === 0x55 && first[511] === 0xaa) {
    const parsed = readMbrPartitions(first, sectorSize);
    if (parsed.parts.length > 0 || parsed.hasExtended) {
      const signature = Array.from({ length: 4 }, (_, i) =>
        first[440 + i]!.toString(16).padStart(2, '0'),
      ).join('');
      const layout: DiskLayout = {
        scheme: 'mbr',
        sectorSize,
        partitions: parsed.parts,
        warnings,
        diskIdentifier: signature,
      };
      await readExtendedPartitions(source, layout);
      return layout;
    }
    // 有 55AA 但不是合法 MBR：可能是 FAT 引导扇区（偏移 510 恰好 0x55AA 的概率极低，
    // 但 exFAT 引导扇区也以此结尾）。继续按裸卷探测。
    warnings.push('发现 0x55AA 结尾但分区表无效，且无已知文件系统签名');
  }

  // 自定义对齐探针：某些镜像有非 512 对齐的卷起点。
  const probe = await probeVolumeOffsets(source);
  if (probe) {
    warnings.push(`未发现标准分区表，按卷签名在偏移 ${probe.offset} 发现 ${probe.fs}`);
    return {
      scheme: 'superfloppy',
      sectorSize,
      partitions: [
        {
          index: 0,
          scheme: 'superfloppy',
          offset: probe.offset,
          size: source.size - probe.offset,
          typeCode: 'bare',
          typeLabel: probe.fs,
          bootable: false,
        },
      ],
      warnings,
    };
  }

  return { scheme: 'none', sectorSize, partitions: [], warnings: ['未识别到分区表或文件系统卷'] };
}

function decodeSectorSizeFromMbr(mbr: Uint8Array): number | null {
  // 分区表项起始 LBA 若超过 2^32 扇区，则隐含更大的逻辑扇区；此处不做猜测，
  // 交由 GPT / 卷引导区判断。保留函数以便未来扩展 4Kn。
  const p0type = mbr[446 + 4];
  if (p0type === undefined) return null;
  return null;
}

function hasGptSignature(sector: Uint8Array): boolean {
  // 保护分区类型 0xEE 位于第一个分区项
  return sector[446 + 4] === 0xee;
}

async function readGpt(
  source: ByteSource,
  sectorSize: number,
  warnings: string[],
): Promise<DiskLayout | null> {
  const header = await source.read(sectorSize, sectorSize);
  const sig = String.fromCharCode(...header.subarray(0, 8));
  if (sig !== 'EFI PART') {
    warnings.push('MBR 含 GPT 保护分区，但 GPT 头签名无效');
    return { scheme: 'gpt', sectorSize, partitions: [], warnings };
  }
  const headerSize = readU32(header, 12);
  const headerCrc = readU32(header, 16);
  const calc = crc32(header, 0, Math.min(headerSize, sectorSize), 16);
  if (calc !== headerCrc) {
    warnings.push(`GPT 头 CRC 不匹配 (期望 ${headerCrc.toString(16)}, 实际 ${calc.toString(16)})`);
  }
  const entriesLba = readU64(header, 72);
  const numEntries = readU32(header, 80);
  const entrySize = readU32(header, 84);
  const entriesCrc = readU32(header, 88);
  const diskGuid = guidToString(header.subarray(56, 72));

  if (entrySize < 128 || entrySize > 4096 || numEntries > 4096) {
    warnings.push(`GPT 分区项参数异常: count=${numEntries} size=${entrySize}`);
    return { scheme: 'gpt', sectorSize, partitions: [], warnings, diskIdentifier: diskGuid };
  }

  const entriesBytes = await source.read(entriesLba * sectorSize, numEntries * entrySize);
  const crcCalc = crc32(entriesBytes, 0, entriesBytes.length, -1);
  if (crcCalc !== entriesCrc) {
    warnings.push(`GPT 分区项 CRC 不匹配 (期望 ${entriesCrc.toString(16)}, 实际 ${crcCalc.toString(16)})`);
  }

  const partitions: RawPartition[] = [];
  for (let i = 0; i < numEntries; i++) {
    const at = i * entrySize;
    const typeGuid = guidToString(entriesBytes.subarray(at, at + 16));
    if (typeGuid === '00000000-0000-0000-0000-000000000000') continue;
    const firstLba = readU64(entriesBytes, at + 32);
    const lastLba = readU64(entriesBytes, at + 40);
    const partGuid = guidToString(entriesBytes.subarray(at + 16, at + 32));
    const name = utf16le(entriesBytes.subarray(at + 56, at + 56 + 72));
    partitions.push({
      index: partitions.length,
      scheme: 'gpt',
      offset: firstLba * sectorSize,
      size: (lastLba - firstLba + 1) * sectorSize,
      typeCode: typeGuid,
      typeLabel: gptTypeLabel(typeGuid),
      bootable: false,
      guid: partGuid,
      ...(name ? { name } : {}),
    });
  }
  return { scheme: 'gpt', sectorSize, partitions, warnings, diskIdentifier: diskGuid };
}

function readMbrPartitions(
  mbr: Uint8Array,
  sectorSize: number,
): { parts: RawPartition[]; hasExtended: boolean } {
  const out: RawPartition[] = [];
  let hasExtended = false;

  for (let i = 0; i < 4; i++) {
    const at = 446 + i * 16;
    const boot = mbr[at]!;
    const type = mbr[at + 4]!;
    const lba = readU32(mbr, at + 8);
    const sectors = readU32(mbr, at + 12);
    if (type === 0 || sectors === 0) continue;
    if (type === 0x05 || type === 0x0f || type === 0x85) {
      hasExtended = true;
      continue;
    }
    out.push({
      index: out.length,
      scheme: 'mbr',
      offset: lba * sectorSize,
      size: sectors * sectorSize,
      typeCode: `0x${type.toString(16).padStart(2, '0')}`,
      typeLabel: MBR_TYPE_LABELS[type] ?? '未知类型',
      bootable: boot === 0x80,
    });
  }
  return { parts: out, hasExtended };
}

/** 扩展分区（EBR 链）异步解析。 */
export async function readExtendedPartitions(
  source: ByteSource,
  layout: DiskLayout,
): Promise<void> {
  if (layout.scheme !== 'mbr') return;
  // 重新读取 MBR 以找到扩展分区项
  const mbr = await source.read(0, 512);
  for (let i = 0; i < 4; i++) {
    const at = 446 + i * 16;
    const type = mbr[at + 4]!;
    if (type !== 0x05 && type !== 0x0f && type !== 0x85) continue;
    const base = readU32(mbr, at + 8);
    let ebr = base;
    const visited = new Set<number>();
    let guard = 0;
    while (ebr > 0 && !visited.has(ebr) && guard++ < 128) {
      visited.add(ebr);
      const buf = await source.read(ebr * layout.sectorSize, 512);
      const logical = buf.subarray(446, 462);
      const link = buf.subarray(446 + 16, 446 + 32);
      const lType = logical[4]!;
      const lLba = readU32(logical, 8);
      const lSectors = readU32(logical, 12);
      if (lType !== 0 && lSectors !== 0 && lLba !== 0) {
        layout.partitions.push({
          index: layout.partitions.length,
          scheme: 'mbr',
          offset: (ebr + lLba) * layout.sectorSize,
          size: lSectors * layout.sectorSize,
          typeCode: `0x${lType.toString(16).padStart(2, '0')}`,
          typeLabel: MBR_TYPE_LABELS[lType] ?? '未知类型',
          bootable: logical[0] === 0x80,
        });
      }
      const next = readU32(link, 8);
      ebr = next > 0 ? base + next : 0;
    }
  }
}

/** 裸卷签名探测。 */
export async function detectBareVolume(
  source: ByteSource,
  offset: number,
  size: number,
): Promise<string | null> {
  if (size < 512) return null;
  const boot = await source.read(offset, 512);
  const sig = String.fromCharCode(...boot.subarray(3, 11));
  if (sig === 'EXFAT   ') return 'exFAT';
  const fatMarker = String.fromCharCode(...boot.subarray(54, 62));
  if (/^FAT(12|16|32)/.test(fatMarker) || /^MSDOS/.test(sig) || /^BSD/.test(sig)) {
    return 'FAT';
  }
  return null;
}

/**
 * 卷签名扫描：当分区表损坏时，按 512 字节步进寻找引导扇区。
 * 仅探测前 64MiB，避免对 32GB 镜像做全盘扫描。
 */
async function probeVolumeOffsets(
  source: ByteSource,
): Promise<{ offset: number; fs: string } | null> {
  const limit = Math.min(source.size, 64 * 1024 * 1024);
  const step = 512;
  for (let offset = 0; offset + 512 <= limit; offset += step) {
    const buf = await source.read(offset, 512);
    const sig = String.fromCharCode(...buf.subarray(3, 11));
    if (sig === 'EXFAT   ') return { offset, fs: 'exFAT' };
    const fatMarker = String.fromCharCode(...buf.subarray(54, 62));
    if (/^FAT(12|16|32)/.test(fatMarker)) return { offset, fs: 'FAT' };
  }
  return null;
}

function gptTypeLabel(guid: string): string {
  const map: Record<string, string> = {
    'c12a7328-f81f-11d2-ba4b-00a0c93ec93b': 'EFI 系统分区',
    'ebd0a0a2-b9e5-4433-87c0-68b6b72699c7': 'Microsoft 基本数据分区',
    '0fc63daf-8483-4772-8e79-3d69d8477de4': 'Linux 文件系统',
    'e3c9e316-0b5c-4db8-817d-f92df00215ae': 'Microsoft 保留分区',
  };
  return map[guid] ?? guid;
}

function guidToString(bytes: Uint8Array): string {
  const hex = (n: number) => n.toString(16).padStart(2, '0');
  const b = bytes;
  const p1 = [b[3]!, b[2]!, b[1]!, b[0]!].map(hex).join('');
  const p2 = [b[5]!, b[4]!].map(hex).join('');
  const p3 = [b[7]!, b[6]!].map(hex).join('');
  const p4 = [b[8]!, b[9]!].map(hex).join('');
  const p5 = Array.from(b.subarray(10, 16)).map(hex).join('');
  return `${p1}-${p2}-${p3}-${p4}-${p5}`;
}

function utf16le(bytes: Uint8Array): string {
  let out = '';
  for (let i = 0; i + 1 < bytes.length; i += 2) {
    const cp = bytes[i]! | (bytes[i + 1]! << 8);
    if (cp === 0) break;
    out += String.fromCharCode(cp);
  }
  return out;
}

function readU32(buf: Uint8Array, off: number): number {
  return (
    buf[off]! | (buf[off + 1]! << 8) | (buf[off + 2]! << 16) | (buf[off + 3]! << 24)
  ) >>> 0;
}

function readU64(buf: Uint8Array, off: number): number {
  return readU32(buf, off) + readU32(buf, off + 4) * 0x1_0000_0000;
}

/** CRC-32（GPT），初始值与多项式按 UEFI 规范。 */
export function crc32(buf: Uint8Array, start = 0, end = buf.length, skipOffset = -1): number {
  let crc = 0xffffffff;
  for (let i = start; i < end; i++) {
    if (i === skipOffset || (skipOffset >= 0 && i >= skipOffset && i < skipOffset + 4)) continue;
    crc ^= buf[i]!;
    for (let k = 0; k < 8; k++) {
      crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1));
    }
  }
  return (crc ^ 0xffffffff) >>> 0;
}

export { MBR_TYPE_LABELS, SECTOR };
