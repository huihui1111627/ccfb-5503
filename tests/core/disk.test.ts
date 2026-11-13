/**
 * 分区表（MBR / GPT）与镜像只读性测试。
 */
import { describe, expect, it } from 'vitest';
import { MemorySource } from '../../src/core/source';
import { detectDiskLayout, crc32 } from '../../src/core/disk';
import { openImage } from '../../src/core/analyze';
import { buildFat32 } from '../fixtures/fat32-builder';

const t = (s: string): Date => new Date(s);

function emptyFat(): Uint8Array {
  return buildFat32({
    sizeBytes: 16 * 1024 * 1024,
    sectorsPerCluster: 1,
    files: [
      { shortName: 'A.TXT', name: 'A.TXT', size: 100, created: t('2026-01-01T00:00:00Z'), modified: t('2026-01-01T00:00:00Z'), accessed: t('2026-01-01T00:00:00Z') },
    ],
  }).bytes;
}

/** 把卷嵌入一个 MBR 分区（分区起点 2048 扇区）。 */
function wrapMbr(volume: Uint8Array, opts: { startSector?: number } = {}): Uint8Array {
  const startSector = opts.startSector ?? 2048;
  const disk = new Uint8Array((startSector + Math.ceil(volume.length / 512)) * 512);
  disk.set(volume, startSector * 512);
  const mbr = disk.subarray(0, 512);
  mbr[446] = 0x80; // bootable
  mbr[450] = 0x0c; // FAT32 LBA
  writeLe32(mbr, 454, startSector);
  writeLe32(mbr, 458, Math.ceil(volume.length / 512));
  mbr[510] = 0x55;
  mbr[511] = 0xaa;
  return disk;
}

function writeLe32(buf: Uint8Array, off: number, v: number): void {
  buf[off] = v & 0xff;
  buf[off + 1] = (v >>> 8) & 0xff;
  buf[off + 2] = (v >>> 16) & 0xff;
  buf[off + 3] = (v >>> 24) & 0xff;
}

function writeLe64(buf: Uint8Array, off: number, v: number): void {
  writeLe32(buf, off, v % 0x1_0000_0000);
  writeLe32(buf, off + 4, Math.floor(v / 0x1_0000_0000));
}

/** 构造带正确 CRC 的 GPT 磁盘。 */
function wrapGpt(volume: Uint8Array): Uint8Array {
  const sector = 512;
  const total = 128 + Math.ceil(volume.length / sector) + 34;
  const disk = new Uint8Array(total * sector);
  disk.set(volume, 128 * sector);

  // 保护 MBR
  const mbr = disk.subarray(0, 512);
  mbr[446 + 4] = 0xee;
  writeLe32(mbr, 446 + 8, 1);
  writeLe32(mbr, 446 + 12, total - 1);
  mbr[510] = 0x55; mbr[511] = 0xaa;

  // 分区项
  const entrySector = 2;
  const entries = disk.subarray(entrySector * sector, entrySector * sector + 128 * 128);
  const typeGuid = hexToGuid('ebd0a0a2-b9e5-4433-87c0-68b6b72699c7');
  entries.set(typeGuid, 0);
  entries.set(hexToGuid('11111111-2222-3333-4444-555555555555'), 16);
  writeLe64(entries, 32, 128);
  writeLe64(entries, 40, 128 + Math.ceil(volume.length / sector) - 1);
  const name = 'DASHCAM';
  for (let i = 0; i < name.length; i++) entries[56 + i * 2] = name.charCodeAt(i);
  const entriesCrc = crc32(entries, 0, entries.length, -1);

  // GPT 头
  const header = disk.subarray(sector, sector * 2);
  header.set(new TextEncoder().encode('EFI PART'), 0);
  writeLe32(header, 8, 0x00010000);
  writeLe32(header, 12, 92);
  writeLe64(header, 24, 1);
  writeLe64(header, 32, total - 1);
  writeLe64(header, 40, 34);
  writeLe64(header, 48, total - 34);
  header.set(hexToGuid('aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'), 56);
  writeLe64(header, 72, entrySector);
  writeLe32(header, 80, 128);
  writeLe32(header, 84, 128);
  writeLe32(header, 88, entriesCrc);
  const headerCrc = crc32(header, 0, 92, 16);
  writeLe32(header, 16, headerCrc);
  return disk;
}

function hexToGuid(guid: string): Uint8Array {
  const [p1, p2, p3, p4, p5] = guid.split('-');
  const b = new Uint8Array(16);
  const p1h = p1!.match(/../g)!.map((x) => parseInt(x, 16));
  b[0] = p1h[3]!; b[1] = p1h[2]!; b[2] = p1h[1]!; b[3] = p1h[0]!;
  const p2h = p2!.match(/../g)!.map((x) => parseInt(x, 16));
  b[4] = p2h[1]!; b[5] = p2h[0]!;
  const p3h = p3!.match(/../g)!.map((x) => parseInt(x, 16));
  b[6] = p3h[1]!; b[7] = p3h[0]!;
  const p4h = p4!.match(/../g)!.map((x) => parseInt(x, 16));
  b[8] = p4h[0]!; b[9] = p4h[1]!;
  const p5h = p5!.match(/../g)!.map((x) => parseInt(x, 16));
  for (let i = 0; i < 6; i++) b[10 + i] = p5h[i]!;
  return b;
}

describe('MBR', () => {
  it('识别 MBR 单分区并打开其中的 FAT32 卷', async () => {
    const volume = emptyFat();
    const disk = wrapMbr(volume);
    const layout = await detectDiskLayout(new MemorySource(disk));
    expect(layout.scheme).toBe('mbr');
    expect(layout.partitions).toHaveLength(1);
    expect(layout.partitions[0]!.offset).toBe(2048 * 512);

    const image = await openImage(new MemorySource(disk));
    expect(image.volumes).toHaveLength(1);
    expect(image.volumes[0]!.info.fs).toBe('fat32');
    expect(image.volumes[0]!.info.offset).toBe(2048 * 512);
  });

  it('非零分区起点时所有绝对偏移正确', async () => {
    const volume = emptyFat();
    const disk = wrapMbr(volume, { startSector: 512 });
    const image = await openImage(new MemorySource(disk));
    const handle = image.volumes[0]!;
    expect(handle.info.dataOffset).toBeGreaterThan(512 * 512);
    const scan = await handle.reader.listEntries({ includeDeleted: true, maxEntries: 100 });
    expect(scan.entries.find((e) => e.name === 'A.TXT')).toBeDefined();
    expect(scan.entries[0]!.entryOffset).toBeGreaterThan(512 * 512);
  });
});

describe('GPT', () => {
  it('识别 GPT、校验 CRC 并打开分区卷', async () => {
    const volume = emptyFat();
    const disk = wrapGpt(volume);
    const layout = await detectDiskLayout(new MemorySource(disk));
    expect(layout.scheme).toBe('gpt');
    expect(layout.partitions).toHaveLength(1);
    expect(layout.partitions[0]!.offset).toBe(128 * 512);
    expect(layout.partitions[0]!.name).toBe('DASHCAM');
    expect(layout.warnings.filter((w) => /CRC/.test(w))).toEqual([]);

    const image = await openImage(new MemorySource(disk));
    expect(image.volumes[0]?.info.fs).toBe('fat32');
  });

  it('GPT 分区项 CRC 被破坏时发出警告但仍尽力解析', async () => {
    const disk = wrapGpt(emptyFat());
    disk[2 * 512 + 40] = 0xff; // 破坏分区项内容
    const layout = await detectDiskLayout(new MemorySource(disk));
    expect(layout.scheme).toBe('gpt');
    expect(layout.warnings.join(' ')).toMatch(/CRC/);
  });
});

describe('镜像只读', () => {
  it('对磁盘镜像做完整解析不会修改源字节', async () => {
    const disk = wrapMbr(emptyFat());
    const snapshot = Uint8Array.from(disk);
    const source = new MemorySource(disk);
    await detectDiskLayout(source);
    const image = await openImage(source);
    await image.volumes[0]!.reader.listEntries({ includeDeleted: true, maxEntries: 1000 });
    expect(Buffer.from(disk).equals(Buffer.from(snapshot))).toBe(true);
  });

  it('BlobSource 读取与 MemorySource 一致', async () => {
    const disk = wrapMbr(emptyFat());
    const { BlobSource } = await import('../../src/core/source');
    const blob = new Blob([disk as unknown as BlobPart]);
    const blobSource = new BlobSource(blob);
    const memSource = new MemorySource(disk);
    expect(blobSource.size).toBe(memSource.size);
    const a = await blobSource.read(1000, 4096);
    const b = await memSource.read(1000, 4096);
    expect(Buffer.from(a).equals(Buffer.from(b))).toBe(true);
  });
});

describe('体积边界', () => {
  it('小容量卷仍按常规路径读取 FAT', async () => {
    const bytes = emptyFat();
    const source = new MemorySource(bytes);
    const layout = await detectDiskLayout(source);
    expect(layout.scheme).toBe('superfloppy');
  });
});
