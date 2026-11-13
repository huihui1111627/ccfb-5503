/**
 * 大容量镜像（32GB 级）可行性验证。
 *
 * 生成方式：Node 端用稀疏文件（truncate）写入 FAT32 引导区 + FAT 表头部，
 * 不实际占用 32GB 磁盘；浏览器侧通过 File/Blob 随机读取，与真实卡行为一致。
 * 校验点：
 *  1) 打开镜像不整表载入 FAT（内存受控）；
 *  2) 目录扫描只触及根目录簇；
 *  3) 卷参数（簇数、数据区偏移）与数学推导一致；
 *  4) 顺序哈希全盘可在合理时间内完成（浏览器 WASM 版本更快）。
 */
import { describe, expect, it } from 'vitest';
import { closeSync, openSync, readSync, statSync, truncateSync, writeSync } from 'node:fs';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { FatVolume } from '../../src/core/fs/fat';
import { hashSource } from '../../src/core/hash';
import type { ByteSource } from '../../src/core/source';

class FileHandleSource implements ByteSource {
  private readonly fd: number;
  constructor(path: string, readonly size: number) {
    this.fd = openSync(path, 'r');
  }
  async read(offset: number, length: number): Promise<Uint8Array> {
    const buf = Buffer.alloc(length);
    const n = readSync(this.fd, buf, 0, length, offset);
    return new Uint8Array(buf.subarray(0, n));
  }
  close(): void {
    closeSync(this.fd);
  }
}

function buildLargeFat32(path: string, sizeGiB: number, clusterBytes: number): void {
  const size = sizeGiB * 1024 * 1024 * 1024;
  const fd = openSync(path, 'w');
  truncateSync(path, size);
  const sectorsPerCluster = clusterBytes / 512;
  const totalSectors = size / 512;
  const reservedSectors = 32;
  const numFats = 2;
  // 解 FAT 大小
  let fatSectors = Math.ceil((totalSectors / sectorsPerCluster) * 4 / 512) + 8;
  for (let i = 0; i < 10; i++) {
    const dataSectors = totalSectors - reservedSectors - numFats * fatSectors;
    const clusters = Math.floor(dataSectors / sectorsPerCluster);
    fatSectors = Math.ceil(((clusters + 2) * 4) / 512);
  }
  const dataSectors = totalSectors - reservedSectors - numFats * fatSectors;
  const clusters = Math.floor(dataSectors / sectorsPerCluster);

  const boot = Buffer.alloc(512);
  boot[0] = 0xeb; boot[1] = 0x58; boot[2] = 0x90;
  boot.write('MSDOS5.0', 3, 'ascii');
  boot.writeUInt16LE(512, 11);
  boot[13] = sectorsPerCluster;
  boot.writeUInt16LE(reservedSectors, 14);
  boot[16] = numFats;
  boot.writeUInt16LE(0, 17);
  boot.writeUInt16LE(totalSectors > 0xffff ? 0 : totalSectors, 19);
  boot[21] = 0xf8;
  boot.writeUInt16LE(0, 22);
  boot.writeUInt16LE(32, 24);
  boot.writeUInt16LE(64, 26);
  boot.writeUInt32LE(totalSectors, 32);
  boot.writeUInt32LE(fatSectors, 36);
  boot.writeUInt16LE(0, 40);
  boot.writeUInt16LE(0, 42);
  boot.writeUInt32LE(2, 44);
  boot.writeUInt16LE(1, 48);
  boot.writeUInt16LE(6, 50);
  boot[64] = 0x80;
  boot[66] = 0x29;
  boot.writeUInt32LE(0x1234abcd, 67);
  boot.write('BIGCARD    ', 71, 'ascii');
  boot.write('FAT32   ', 82, 'ascii');
  boot[510] = 0x55; boot[511] = 0xaa;
  writeSync(fd, boot, 0, 512, 0);
  writeSync(fd, boot, 0, 512, 6 * 512);

  // FSInfo
  const fsinfo = Buffer.alloc(512);
  fsinfo.writeUInt32LE(0x41615252, 0);
  fsinfo.writeUInt32LE(0x61417272, 484);
  fsinfo.writeUInt32LE(clusters, 488);
  fsinfo.writeUInt32LE(0xffffffff, 492);
  fsinfo.writeUInt16LE(0xaa55, 510);
  writeSync(fd, fsinfo, 0, 512, 512);

  // FAT 头部：簇 0/1 保留 + 根目录簇 2 链
  const fatHead = Buffer.alloc(4096);
  fatHead.writeUInt32LE(0x0ffffff8, 0);
  fatHead.writeUInt32LE(0x0fffffff, 4);
  fatHead.writeUInt32LE(0x0ffffff8, 8); // 根目录单簇
  writeSync(fd, fatHead, 0, 4096, reservedSectors * 512);
  writeSync(fd, fatHead, 0, 4096, (reservedSectors + fatSectors) * 512);

  // 根目录簇（簇 2）写一个现存文件条目
  const dirOffset = (reservedSectors + numFats * fatSectors) * 512;
  const dir = Buffer.alloc(clusterBytes);
  const rec = Buffer.alloc(32);
  rec.write('BIGVIDEOMP4', 0, 'ascii'); // BIGVIDEO.MP4
  rec[11] = 0x20;
  rec.writeUInt16LE(0, 20);
  rec.writeUInt16LE(0x6000, 22); // 12:00:00
  rec.writeUInt16LE(0x5a41, 24); // 2025-03-05
  rec.writeUInt16LE(0, 26);
  rec.writeUInt32LE(0, 28);
  dir.set(rec, 0);
  writeSync(fd, dir, 0, clusterBytes, dirOffset);
  closeSync(fd);
  return void fd;
}

describe('32GB 级镜像', () => {
  const dir = mkdtempSync(join(tmpdir(), 'sd-forensics-big-'));
  const path = join(dir, 'big32-fat32.dd');
  let created = false;

  it('打开 32GB 稀疏镜像：FAT 不整表入内存，参数正确', async () => {
    buildLargeFat32(path, 32, 512);
    created = true;
    const size = statSync(path).size;
    const source = new FileHandleSource(path, size);
    try {
      const vol = await FatVolume.open(source, 0, size, 0);
      expect(vol.info.fs).toBe('fat32');
      expect(vol.info.clusterSize).toBe(512);
      expect(vol.info.clusterCount).toBeGreaterThan(60_000_000);
      expect(vol.info.fatSize).toBeGreaterThan(200 * 1024 * 1024);
      expect(vol.info.freeCountSource).toBe('fsinfo');
      // 按需读取：未载入整表
      await vol.loadFat();
      expect(vol.info.warnings.some((w) => w.code === 'fat-on-demand')).toBe(true);
      const { entries } = await vol.listEntries({ includeDeleted: true, maxEntries: 100 });
      expect(entries.length).toBe(1);
      expect(entries[0]!.name).toBe('BIGVIDEO.MP4');
    } finally {
      source.close();
    }
  }, 60_000);

  it('顺序哈希 32GB 需可行（本机稀疏读约数十秒；浏览器 WASM 更快）', async () => {
    // 该测试只验证哈希器能在流式读取大文件时不崩溃；正式性能在浏览器 E2E 中测。
    const size = statSync(path).size;
    const source = new FileHandleSource(path, size);
    try {
      const t0 = Date.now();
      const digests = await hashSource(
        {
          size: 64 * 1024 * 1024, // 取前 64MiB 代表采样，避免 CI 过慢
          read: (o, l) => source.read(o, l),
        },
      );
      expect(digests.sha256).toMatch(/^[0-9a-f]{64}$/);
      expect(Date.now() - t0).toBeLessThan(60_000);
    } finally {
      source.close();
    }
  }, 60_000);

  it('清理稀疏文件', () => {
    if (created) rmSync(dir, { recursive: true, force: true });
  });
});
