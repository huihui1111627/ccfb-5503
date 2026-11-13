/**
 * FAT32 测试镜像生成器（独立实现，不使用被测解析器）。
 *
 * 场景支持：
 *  - 现存文件（连续 / 碎片化 / 子目录 / 长文件名）
 *  - 已删除文件，LFN 残留、FAT 链保留（可完整恢复）
 *  - 已删除文件，FAT 链被清零（按连续簇恢复）
 *  - 已删除后数据簇被新文件覆盖（应标记不完整/覆盖）
 *  - 已删除目录及其中的文件
 *  - 时间戳精确可控
 */

import { createHash } from 'node:crypto';

export interface Fat32FileSpec {
  /** 长文件名（不填则用 shortName）。 */
  name?: string;
  shortName: string;
  /** 所在目录路径，'' 表示根目录。 */
  dir?: string;
  size: number;
  /** 数据填充种子；默认按名称派生。 */
  seed?: number;
  /** 数据内容（优先于 seed + size）。 */
  content?: Uint8Array;
  created: Date;
  modified: Date;
  accessed: Date;
  /** 删除该条目（写 0xE5 / 清 FAT）。 */
  deleted?: boolean;
  /** 删除时清零 FAT 链（模拟相机/系统删除行为）。 */
  clearFatOnDelete?: boolean;
  /** 删除后其簇被后续文件覆盖的字节数（写入新数据）。 */
  overwriteBytesAfterDelete?: number;
  /** 碎片化：给定簇数量，交替写入以形成非连续链。 */
  fragment?: boolean;
}

export interface Fat32DirSpec {
  name: string;
  dir?: string;
  deleted?: boolean;
  created: Date;
  modified: Date;
  accessed: Date;
}

export interface Fat32CaseSpec {
  label?: string;
  serial?: number;
  /** 卷大小（字节，默认 64MiB）。 */
  sizeBytes?: number;
  /** 每簇扇区数（默认 1 = 512B 簇，便于精确构造碎链）。 */
  sectorsPerCluster?: number;
  timezoneOffsetMinutes?: number;
  dirs?: Fat32DirSpec[];
  files: Fat32FileSpec[];
  /** 生成后在卷尾填充随机未分配数据（用于雕刻负样本）。 */
  noiseClusters?: number;
  /** 显式保留的“空闲”簇号（用于控制分配布局）。 */
  reservedFreeClusters?: number[];
}

export interface Fat32BuildResult {
  bytes: Uint8Array;
  /** 已知答案，供测试断言。 */
  expectations: Fat32Expectations;
}

export interface Fat32Expectations {
  files: Array<{
    path: string;
    name: string;
    deleted: boolean;
    size: number;
    sha256: string;
    md5: string;
    firstCluster: number;
    clusters: number[];
    expectStatus: 'complete' | 'partial' | 'overwritten';
    overwrittenClusters: number;
    createdUtc: string;
    modifiedUtc: string;
    accessedUtc: string;
  }>;
  dirs: Array<{ path: string; deleted: boolean }>;
  volume: {
    totalClusters: number;
    freeClusters: number;
    clusterSize: number;
    label: string;
  };
  /** 生成时写入的原始卷字节哈希（用于“分析前后哈希一致”基线）。 */
  imageSha256: string;
  imageMd5: string;
}

class ByteWriter {
  readonly buf: Uint8Array;
  constructor(size: number) {
    this.buf = new Uint8Array(size);
  }
  u8(off: number, v: number): void {
    this.buf[off] = v & 0xff;
  }
  u16(off: number, v: number): void {
    this.buf[off] = v & 0xff;
    this.buf[off + 1] = (v >>> 8) & 0xff;
  }
  u32(off: number, v: number): void {
    this.buf[off] = v & 0xff;
    this.buf[off + 1] = (v >>> 8) & 0xff;
    this.buf[off + 2] = (v >>> 16) & 0xff;
    this.buf[off + 3] = (v >>> 24) & 0xff;
  }
  set(off: number, data: Uint8Array): void {
    this.buf.set(data, off);
  }
  fill(off: number, len: number, v: number): void {
    this.buf.fill(v, off, off + len);
  }
}

interface DirHandle {
  path: string;
  firstCluster: number;
  clusterBytes: Map<number, Uint8Array>;
  entriesOffset: number;
  dir: boolean;
}

interface PendingFile {
  spec: Fat32FileSpec;
  clusters: number[];
  content: Uint8Array;
  dirPath: string;
  shortEntryOffset: number;
  lfnOffsets: number[];
  overriddenCluster?: number;
}

const FAT_EOC = 0x0ffffff8;

export function buildFat32(spec: Fat32CaseSpec): Fat32BuildResult {
  const sectorsPerCluster = spec.sectorsPerCluster ?? 1;
  const clusterSize = 512 * sectorsPerCluster;
  const sizeBytes = spec.sizeBytes ?? 64 * 1024 * 1024;
  const totalSectors = Math.floor(sizeBytes / 512);
  const reservedSectors = 32;
  const numFats = 2;

  // 迭代求解 FAT 大小（使首数据扇区 + 簇数 == 总扇区）。
  let fatSectors = 128;
  for (let iter = 0; iter < 20; iter++) {
    const dataSectors = totalSectors - reservedSectors - numFats * fatSectors;
    const clusters = Math.floor(dataSectors / sectorsPerCluster);
    const need = Math.ceil(((clusters + 2) * 4) / 512);
    if (need === fatSectors) break;
    fatSectors = need;
  }
  const dataSectors = totalSectors - reservedSectors - numFats * fatSectors;
  const totalClusters = Math.floor(dataSectors / sectorsPerCluster);
  const firstDataSector = reservedSectors + numFats * fatSectors;
  const fatOffset = reservedSectors * 512;
  const dataOffset = firstDataSector * 512;

  const writer = new ByteWriter(totalSectors * 512);
  const image = writer.buf;

  // --- 引导扇区 ---
  writeFat32BootSector(writer, {
    sectorsPerCluster,
    reservedSectors,
    numFats,
    totalSectors,
    fatSectors,
    rootCluster: 2,
    label: spec.label ?? 'DASHCAM',
    serial: spec.serial ?? 0x1a2b3c4d,
    clusterCount: totalClusters,
  });
  // 备份引导扇区（扇区 6）
  image.copyWithin(6 * 512, 0, 512);

  // FSInfo（扇区 1）
  const fsinfoOff = 512;
  writer.u32(fsinfoOff, 0x41615252);
  writer.u32(fsinfoOff + 484, 0x61417272);
  writer.u32(fsinfoOff + 488, totalClusters); // 稍后修正空闲数
  writer.u32(fsinfoOff + 492, 0xffffffff);
  writer.u16(fsinfoOff + 510, 0xaa55);
  image.copyWithin(504 * 512, 512, 1024); // 备份 FSInfo 于扇区 7

  // --- FAT ---
  const fat = new Uint8Array(fatSectors * 512);
  const setFat = (cluster: number, value: number): void => {
    fat[cluster * 4] = value & 0xff;
    fat[cluster * 4 + 1] = (value >>> 8) & 0xff;
    fat[cluster * 4 + 2] = (value >>> 16) & 0xff;
    fat[cluster * 4 + 3] = (value >>> 24) & 0xff;
  };
  const getFat = (cluster: number): number =>
    (fat[cluster * 4]! |
      (fat[cluster * 4 + 1]! << 8) |
      (fat[cluster * 4 + 2]! << 16) |
      (fat[cluster * 4 + 3]! << 24)) >>> 0;
  setFat(0, 0x0ffffff8);
  setFat(1, 0x0fffffff);

  const clusterOffset = (cluster: number): number => dataOffset + (cluster - 2) * clusterSize;

  // 分配器状态
  let nextFree = 2;
  const allocated = new Set<number>();

  const allocateContiguous = (count: number): number[] => {
    const out: number[] = [];
    for (let c = nextFree; c < totalClusters + 2 && out.length < count; c++) {
      if (!allocated.has(c)) {
        out.push(c);
        allocated.add(c);
      }
    }
    if (out.length < count) throw new Error(`空间不足: 需要 ${count} 簇，仅得 ${out.length}`);
    nextFree = out[out.length - 1]! + 1;
    return out;
  };

  const allocateFragmented = (count: number): number[] => {
    // 交替使用低区与高区，形成 2-3 段碎链
    const out: number[] = [];
    let lo = nextFree;
    let hi = totalClusters + 1;
    let takeLow = true;
    while (out.length < count) {
      let picked = -1;
      if (takeLow) {
        while (lo <= totalClusters + 1 && allocated.has(lo)) lo++;
        if (lo <= totalClusters + 1) picked = lo++;
      } else {
        while (hi >= 2 && allocated.has(hi)) hi--;
        if (hi >= 2 && hi > lo) picked = hi--;
      }
      if (picked < 0) {
        // 退回顺序分配
        while (lo <= totalClusters + 1 && allocated.has(lo)) lo++;
        if (lo > totalClusters + 1) throw new Error('空间不足（碎片分配）');
        picked = lo++;
      }
      out.push(picked);
      allocated.add(picked);
      takeLow = !takeLow;
    }
    nextFree = Math.max(nextFree, lo);
    return out;
  };

  const linkChain = (clusters: number[]): void => {
    for (let i = 0; i < clusters.length; i++) {
      const next = i + 1 < clusters.length ? clusters[i + 1]! : FAT_EOC;
      setFat(clusters[i]!, next);
    }
  };

  // --- 根目录 ---
  const dirs = new Map<string, DirHandle>();
  const rootClusters = allocateContiguous(8);
  linkChain(rootClusters);
  const root: DirHandle = {
    path: '',
    firstCluster: 2,
    clusterBytes: new Map(rootClusters.map((c) => [c, new Uint8Array(clusterSize)])),
    entriesOffset: 0,
    dir: true,
  };
  // 根目录必须从簇 2 开始连续（生成器约定）
  if (rootClusters[0] !== 2) throw new Error('根目录簇号必须从 2 开始');
  dirs.set('', root);

  writeVolumeLabel(root.clusterBytes.get(2)!, spec.label ?? 'DASHCAM', 0);

  const writes: Array<{ offset: number; data: Uint8Array }> = [];

  // --- 创建目录（按给定顺序） ---
  for (const d of spec.dirs ?? []) {
    const parentPath = d.dir ?? '';
    const parent = dirs.get(parentPath);
    if (!parent) throw new Error(`父目录不存在: ${parentPath}`);
    const clusters = allocateContiguous(1);
    linkChain(clusters);
    const bytes = new Uint8Array(clusterSize);
    const name = d.name;
    // 子目录必须含 '.' 与 '..'，否则 fsck 会视为目录损坏。
    writeDotEntries(bytes, clusters[0]!, parent.path === '' ? 0 : parent.firstCluster, d);
    (parent as DirHandle & { entriesOffset: number }).entriesOffset += 0;
    const childPath = parentPath === '' ? name : `${parentPath}/${name}`;
    const offset = appendDirEntry(parent, {
      shortName: makeShortName(name),
      longName: name,
      cluster: clusters[0]!,
      size: 0,
      dir: true,
      created: d.created,
      modified: d.modified,
      accessed: d.accessed,
      deleted: d.deleted ?? false,
      timezoneOffsetMinutes: spec.timezoneOffsetMinutes ?? 480,
    });
    void offset;
    const handle: DirHandle = {
      path: childPath,
      firstCluster: clusters[0]!,
      clusterBytes: new Map([[clusters[0]!, bytes]]),
      entriesOffset: clusterOffset(clusters[0]!),
      dir: true,
    };
    dirs.set(childPath, handle);
  }

  // 预分配目录使用的簇
  const usedByDirs = new Set<number>();
  for (const d of dirs.values()) {
    for (const c of d.clusterBytes.keys()) {
      usedByDirs.add(c);
      allocated.add(c);
    }
  }

  // --- 文件 ---
  const pendingFiles: PendingFile[] = [];
  for (const f of spec.files) {
    const dirPath = f.dir ?? '';
    const dir = dirs.get(dirPath);
    if (!dir) throw new Error(`文件所在目录不存在: ${dirPath}`);
    const content = f.content ?? pseudoRandom(f.size, f.seed ?? seedFromName(f.shortName));
    if (content.length !== f.size) throw new Error(`content 长度与 size 不符: ${f.name ?? f.shortName}`);
    const clusterCount = Math.max(1, Math.ceil(f.size / clusterSize));
    let clusters: number[];
    if (f.fragment) {
      clusters = allocateFragmented(clusterCount);
    } else {
      clusters = allocateContiguous(clusterCount);
    }
    if (f.size === 0) clusters = [];
    pendingFiles.push({
      spec: f,
      clusters,
      content,
      dirPath,
      shortEntryOffset: 0,
      lfnOffsets: [],
    });
  }

  // 写入文件数据（先写数据，再写目录项，模拟真实分配顺序）
  for (const pf of pendingFiles) {
    if (pf.clusters.length === 0) continue;
    const totalBytes = pf.clusters.length * clusterSize;
    const blob = new Uint8Array(totalBytes);
    blob.set(pf.content);
    pf.clusters.forEach((c, i) => {
      image.set(blob.subarray(i * clusterSize, (i + 1) * clusterSize), clusterOffset(c));
    });
  }
  // 数据写入后链接链（删除场景随后再处理）
  for (const pf of pendingFiles) {
    if (pf.clusters.length > 0) linkChain(pf.clusters);
  }

  // 写目录项
  const expectations: Fat32Expectations['files'] = [];
  for (const pf of pendingFiles) {
    const dir = dirs.get(pf.dirPath)!;
    const deleted = pf.spec.deleted ?? false;
    const offset = appendDirEntry(dir, {
      shortName: pf.spec.shortName,
      longName: pf.spec.name ?? pf.spec.shortName,
      cluster: pf.clusters[0] ?? 0,
      size: pf.spec.size,
      dir: false,
      created: pf.spec.created,
      modified: pf.spec.modified,
      accessed: pf.spec.accessed,
      deleted,
      timezoneOffsetMinutes: spec.timezoneOffsetMinutes ?? 480,
    });
    pf.shortEntryOffset = offset;
    void deleted;
  }

  // 处理删除语义
  for (const pf of pendingFiles) {
    const deleted = pf.spec.deleted ?? false;
    if (!deleted) continue;
    if (pf.spec.clearFatOnDelete) {
      for (const c of pf.clusters) setFat(c, 0);
    }
    if (pf.spec.overwriteBytesAfterDelete && pf.spec.overwriteBytesAfterDelete > 0) {
      // 真实覆盖：分配一个新的现存文件，占用已删除文件的前 N 个簇。
      // 这样 FAT 链与目录项都会把该簇标记为“已重新分配”，解析器可据此判定覆盖。
      const needClusters = Math.ceil(pf.spec.overwriteBytesAfterDelete / clusterSize);
      const overlap = pf.clusters.slice(0, needClusters);
      const fill = pseudoRandom(overlap.length * clusterSize, 0xdeadbeef ^ pf.spec.seed!);

      // 覆盖数据
      overlap.forEach((c, i) => {
        image.set(fill.subarray(i * clusterSize, (i + 1) * clusterSize), clusterOffset(c));
      });
      // 新文件的链占用这些簇
      linkChain(overlap);
      for (const c of overlap) allocated.add(c);
      // 在根目录追加一个现存条目
      const rootDir = dirs.get('')!;
      appendDirEntry(rootDir, {
        shortName: 'OVR_0001.MP4',
        longName: '覆盖写入的新录像.mp4',
        cluster: overlap[0]!,
        size: overlap.length * clusterSize,
        dir: false,
        created: new Date('2026-03-04T13:00:00Z'),
        modified: new Date('2026-03-04T13:05:00Z'),
        accessed: new Date('2026-03-04T13:05:00Z'),
        deleted: false,
        timezoneOffsetMinutes: spec.timezoneOffsetMinutes ?? 480,
      });
    }
  }

  // --- 噪声（未分配区域随机化，用于雕刻负样本） ---
  if (spec.noiseClusters && spec.noiseClusters > 0) {
    const startCluster = totalClusters + 2 - spec.noiseClusters;
    let x = 0x12345678;
    for (let c = startCluster; c < totalClusters + 2; c++) {
      if (allocated.has(c)) continue;
      const off = clusterOffset(c);
      for (let i = 0; i < clusterSize; i++) {
        x ^= x << 13; x >>>= 0; x ^= x >>> 17; x ^= x << 5; x >>>= 0;
        image[off + i] = x & 0xff;
      }
    }
  }

  // 回写目录簇
  for (const d of dirs.values()) {
    for (const [cluster, bytes] of d.clusterBytes) {
      image.set(bytes, clusterOffset(cluster));
    }
  }

  // 回写 FAT × 2
  image.set(fat, fatOffset);
  image.set(fat, fatOffset + fatSectors * 512);

  // 修正 FSInfo 空闲簇数
  let freeCount = 0;
  for (let c = 2; c < totalClusters + 2; c++) if (getFat(c) === 0) freeCount++;
  writer.u32(fsinfoOff + 488, freeCount);

  // 已知答案
  for (const pf of pendingFiles) {
    const deleted = pf.spec.deleted ?? false;
    const overwrittenClusters = pf.spec.overwriteBytesAfterDelete
      ? Math.ceil(pf.spec.overwriteBytesAfterDelete / clusterSize)
      : 0;
    let expectStatus: 'complete' | 'partial' | 'overwritten';
    if (!deleted) {
      expectStatus = 'complete';
    } else if (overwrittenClusters >= pf.clusters.length && pf.clusters.length > 0) {
      expectStatus = 'overwritten';
    } else if (overwrittenClusters > 0 || pf.spec.size > pf.clusters.length * clusterSize) {
      expectStatus = 'partial';
    } else {
      expectStatus = 'complete';
    }
    expectations.push({
      path: join(pf.dirPath, pf.spec.name ?? pf.spec.shortName),
      name: pf.spec.name ?? pf.spec.shortName,
      deleted,
      size: pf.spec.size,
      sha256: sha256Hex(pf.content),
      md5: md5Hex(pf.content),
      firstCluster: pf.clusters[0] ?? 0,
      clusters: pf.clusters,
      expectStatus,
      overwrittenClusters,
      createdUtc: pf.spec.created.toISOString(),
      modifiedUtc: pf.spec.modified.toISOString(),
      accessedUtc: pf.spec.accessed.toISOString(),
    });
  }

  void writes;
  void usedByDirs;

  const imageSha256 = sha256Hex(image);
  const imageMd5 = md5Hex(image);

  return {
    bytes: image,
    expectations: {
      files: expectations,
      dirs: [...dirs.values()].filter((d) => d.path !== '').map((d) => ({ path: d.path, deleted: false })),
      volume: {
        totalClusters,
        freeClusters: freeCount,
        clusterSize,
        label: spec.label ?? 'DASHCAM',
      },
      imageSha256,
      imageMd5,
    },
  };

  // ---------- 内部函数 ----------

  function appendDirEntry(
    dir: DirHandle,
    e: {
      shortName: string;
      longName: string;
      cluster: number;
      size: number;
      dir: boolean;
      created: Date;
      modified: Date;
      accessed: Date;
      deleted: boolean;
      timezoneOffsetMinutes: number;
    },
  ): number {
    const lfnPieces = needsLfn(e.longName, e.shortName) ? buildLfnPieces(e.longName) : [];
    const needed = lfnPieces.length + 1;
    // 目录末尾有 0x00 结束标记，因此从“已删除”槽位开始才可复用；
    // 为简化生成器，始终在最后一个目录簇末尾追加（必要时扩容）。
    const slot = appendSlot(dir, needed);
    const cluster = slotClusterOf(dir, slot);
    const byteInCluster = slotByteOf(slot);

    // 写入顺序：高序号（LAST）→ … → 序号 1 → 短名项。
    const checksum = shortNameChecksum(e.shortName);
    for (let i = 0; i < lfnPieces.length; i++) {
      const piece = lfnPieces[i]!;
      const ordinal = piece.index + 1;
      const rec = new Uint8Array(32);
      rec[0] = (ordinal | (i === 0 ? 0x40 : 0)) & 0xff;
      rec[11] = 0x0f;
      rec[13] = checksum;
      writeLfnChars(rec, piece.chars);
      if (e.deleted) rec[0] = 0xe5;
      writeToDirCluster(dir, cluster, byteInCluster + i * 32, rec);
    }
    const short = new Uint8Array(32);
    const packedCreated = packTime(e.created, e.timezoneOffsetMinutes);
    const packedModified = packTime(e.modified, e.timezoneOffsetMinutes);
    const packedAccessed = packDate(e.accessed, e.timezoneOffsetMinutes);
    const baseName = shortNameBytes(e.shortName);
    short.set(baseName, 0);
    if (e.deleted) short[0] = 0xe5;
    short[11] = e.dir ? 0x10 : 0x20;
    if (!e.dir) short[11] = 0x20;
    w16(short, 13, packedCreated.tenths);
    w16(short, 14, packedCreated.time);
    w16(short, 16, packedCreated.date);
    w16(short, 18, packedAccessed);
    w16(short, 20, (e.cluster >>> 16) & 0xffff);
    w16(short, 22, packedModified.time);
    w16(short, 24, packedModified.date);
    w16(short, 26, e.cluster & 0xffff);
    w32(short, 28, e.size);
    writeToDirCluster(dir, cluster, byteInCluster + lfnPieces.length * 32, short);
    return 0;
  }

  function appendSlot(dir: DirHandle, needed: number): number {
    // 统计已用槽位（非 0x00 且非 0xE5），并保证有足够连续空槽；不足则扩容。
    const used = countUsedSlots(dir);
    const capacity = dir.clusterBytes.size * (clusterSize / 32);
    while (used + needed + 1 > capacity) {
      const newClusters = allocateContiguous(1);
      const lastCluster = [...dir.clusterBytes.keys()].pop()!;
      setFat(lastCluster, newClusters[0]!);
      setFat(newClusters[0]!, FAT_EOC);
      dir.clusterBytes.set(newClusters[0]!, new Uint8Array(clusterSize));
    }
    return used;
  }

  function countUsedSlots(dir: DirHandle): number {
    let used = 0;
    for (const bytes of dir.clusterBytes.values()) {
      const slots = Math.floor(bytes.length / 32);
      for (let i = 0; i < slots; i++) {
        if (bytes[i * 32] !== 0x00) used++;
      }
    }
    return used;
  }


  function slotClusterOf(dir: DirHandle, globalSlot: number): number {
    const per = Math.floor(clusterSize / 32);
    const keys = [...dir.clusterBytes.keys()];
    return keys[Math.floor(globalSlot / per)]!;
  }

  function slotByteOf(globalSlot: number): number {
    const per = Math.floor(clusterSize / 32);
    return (globalSlot % per) * 32;
  }

  function writeToDirCluster(dir: DirHandle, cluster: number, byteInCluster: number, rec: Uint8Array): void {
    const bytes = dir.clusterBytes.get(cluster)!;
    bytes.set(rec, byteInCluster);
  }

}

function writeDotEntries(
  dirBytes: Uint8Array,
  selfCluster: number,
  parentCluster: number,
  times: { created: Date; modified: Date; accessed: Date },
): void {
  const mk = (name: string, cluster: number, slot: number): void => {
    const rec = dirBytes.subarray(slot * 32, slot * 32 + 32);
    rec.fill(0x20, 0, 11);
    rec[0] = name.charCodeAt(0);
    if (name.length > 1) rec[1] = name.charCodeAt(1);
    rec[11] = 0x10;
    const t = packTime(times.created, 480);
    w16(rec, 13, t.tenths);
    w16(rec, 14, t.time);
    w16(rec, 16, t.date);
    w16(rec, 18, packDate(times.accessed, 480));
    w16(rec, 20, (cluster >>> 16) & 0xffff);
    w16(rec, 22, t.time);
    w16(rec, 24, t.date);
    w16(rec, 26, cluster & 0xffff);
    w32(rec, 28, 0);
  };
  mk('.', selfCluster, 0);
  mk('..', parentCluster, 1);
}

function writeVolumeLabel(dirBytes: Uint8Array, label: string, slot: number): void {
  const rec = dirBytes.subarray(slot * 32, slot * 32 + 32);
  const bytes = asciiBytes(label.toUpperCase(), 11);
  rec.set(bytes, 0);
  rec[11] = 0x08;
}

function writeFat32BootSector(
  w: ByteWriter,
  p: {
    sectorsPerCluster: number;
    reservedSectors: number;
    numFats: number;
    totalSectors: number;
    fatSectors: number;
    rootCluster: number;
    label: string;
    serial: number;
    clusterCount: number;
  },
): void {
  const b = new Uint8Array(512);
  b[0] = 0xeb;
  b[1] = 0x58;
  b[2] = 0x90;
  b.set(asciiBytes('MSDOS5.0', 8), 3);
  w16(b, 11, 512);
  b[13] = p.sectorsPerCluster;
  w16(b, 14, p.reservedSectors);
  b[16] = p.numFats;
  w16(b, 17, 0); // FAT32 根目录项数
  w16(b, 19, p.totalSectors > 0xffff ? 0 : p.totalSectors);
  b[21] = 0xf8;
  w16(b, 22, 0);
  w16(b, 24, 32); // 每道扇区
  w16(b, 26, 64); // 磁头
  w32(b, 28, 0); // 隐藏扇区
  w32(b, 32, p.totalSectors);
  w32(b, 36, p.fatSectors);
  w16(b, 40, 0); // ExtFlags
  w16(b, 42, 0); // FSVer
  w32(b, 44, p.rootCluster);
  w16(b, 48, 1); // FSInfo
  w16(b, 50, 6); // 备份引导扇区
  b[64] = 0x80;
  b[66] = 0x29;
  w32(b, 67, p.serial);
  b.set(asciiBytes(p.label, 11), 71);
  b.set(asciiBytes('FAT32', 8), 82);
  b[510] = 0x55;
  b[511] = 0xaa;
  w.set(0, b);
}

function shortNameBytes(shortName: string): Uint8Array {
  const [baseRaw, extRaw = ''] = shortName.toUpperCase().split('.');
  const base = asciiBytes(baseRaw ?? '', 8);
  const ext = asciiBytes(extRaw, 3);
  const out = new Uint8Array(11);
  out.set(base, 0);
  out.set(ext, 8);
  return out;
}

function makeShortName(longName: string): string {
  const clean = longName.replace(/[^A-Za-z0-9._-]/g, '').toUpperCase();
  const [baseRaw, extRaw] = clean.split('.');
  const base = (baseRaw ?? 'FILE').slice(0, 6) || 'FILE';
  const ext = (extRaw ?? '').slice(0, 3);
  return ext ? `${base}~1.${ext}` : `${base}~1`;
}

function needsLfn(longName: string, shortName: string): boolean {
  return longName.toUpperCase() !== shortName.toUpperCase();
}

function buildLfnPieces(longName: string): Array<{ index: number; chars: string }> {
  // 按 13 个 UTF-16 码元从首字符开始切片：第 i 片序号为 i+1。
  // 返回顺序为磁盘写入顺序：最高序号（LAST）在前，序号 1 紧邻短名项。
  const units = [...longName];
  const count = Math.ceil(units.length / 13);
  const pieces: Array<{ index: number; chars: string }> = [];
  for (let i = 0; i < count; i++) {
    pieces.push({ index: i, chars: units.slice(i * 13, i * 13 + 13).join('') });
  }
  return pieces.reverse();
}

function writeLfnChars(rec: Uint8Array, chars: string): void {
  const offsets = [1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30];
  const units = [...chars];
  for (let i = 0; i < 13; i++) {
    const at = offsets[i]!;
    if (i < units.length) w16(rec, at, units[i]!.charCodeAt(0));
    else if (i === units.length) w16(rec, at, 0x0000);
    else w16(rec, at, 0xffff);
  }
}

function shortNameChecksum(shortName: string): number {
  let sum = 0;
  const bytes = shortNameBytes(shortName);
  for (let i = 0; i < 11; i++) {
    sum = (((sum & 1) << 7) + (sum >>> 1) + bytes[i]!) & 0xff;
  }
  return sum;
}

function packTime(d: Date, tzOffsetMinutes: number): { date: number; time: number; tenths: number } {
  const local = new Date(d.getTime() + tzOffsetMinutes * 60_000);
  const year = local.getUTCFullYear();
  const month = local.getUTCMonth() + 1;
  const day = local.getUTCDate();
  const hour = local.getUTCHours();
  const minute = local.getUTCMinutes();
  const second = local.getUTCSeconds();
  const ms = local.getUTCMilliseconds();
  const date = ((year - 1980) << 9) | (month << 5) | day;
  const time = (hour << 11) | (minute << 5) | Math.floor(second / 2);
  const tenths = Math.min(199, (second % 2) * 100 + Math.floor(ms / 10));
  return { date: date & 0xffff, time: time & 0xffff, tenths };
}

function packDate(d: Date, tzOffsetMinutes: number): number {
  const local = new Date(d.getTime() + tzOffsetMinutes * 60_000);
  return (((local.getUTCFullYear() - 1980) << 9) |
    ((local.getUTCMonth() + 1) << 5) |
    local.getUTCDate()) & 0xffff;
}

function w16(buf: Uint8Array, off: number, v: number): void {
  buf[off] = v & 0xff;
  buf[off + 1] = (v >>> 8) & 0xff;
}

function w32(buf: Uint8Array, off: number, v: number): void {
  buf[off] = v & 0xff;
  buf[off + 1] = (v >>> 8) & 0xff;
  buf[off + 2] = (v >>> 16) & 0xff;
  buf[off + 3] = (v >>> 24) & 0xff;
}

function asciiBytes(s: string, len: number): Uint8Array {
  const out = new Uint8Array(len).fill(0x20);
  for (let i = 0; i < len && i < s.length; i++) out[i] = s.charCodeAt(i) & 0xff;
  return out;
}

function pseudoRandom(length: number, seed: number): Uint8Array {
  const out = new Uint8Array(length);
  let x = seed >>> 0 || 1;
  for (let i = 0; i < length; i++) {
    x ^= x << 13; x >>>= 0;
    x ^= x >>> 17;
    x ^= x << 5; x >>>= 0;
    out[i] = x & 0xff;
  }
  return out;
}

function seedFromName(name: string): number {
  let h = 2166136261;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

function join(dir: string, name: string): string {
  return dir === '' ? `/${name}` : `/${dir}/${name}`;
}

function sha256Hex(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex');
}

function md5Hex(data: Uint8Array): string {
  return createHash('md5').update(data).digest('hex');
}
