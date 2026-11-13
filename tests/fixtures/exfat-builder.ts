/**
 * exFAT 测试镜像生成器（独立实现）。
 *
 * 覆盖场景：
 *  - 连续分配（NoFatChain=1）与 FAT 链式分配
 *  - 已删除条目（仅清 InUse 位，名称字符保留）
 *  - 删除后数据簇被新文件重新分配（分配位图 + 数据覆盖）
 *  - 时间戳带 UTC 偏移与 10ms 增量
 *  - 分配位图、Up-case 表（可省略）
 */
import { createHash } from 'node:crypto';

export interface ExfatFileSpec {
  name: string;
  dir?: string;
  size: number;
  seed?: number;
  content?: Uint8Array;
  created: Date;
  modified: Date;
  accessed: Date;
  deleted?: boolean;
  /** 使用 FAT 链（false/缺省则 NoFatChain=1 连续分配）。 */
  chained?: boolean;
  /** 删除后前 N 字节被新数据覆盖。 */
  overwriteBytesAfterDelete?: number;
  /** 在卷内制造碎片分配（仅链式有效）。 */
  fragment?: boolean;
}

export interface ExfatDirSpec {
  name: string;
  dir?: string;
  deleted?: boolean;
  created: Date;
  modified: Date;
  accessed: Date;
}

export interface ExfatCaseSpec {
  label?: string;
  serial?: number;
  sizeBytes?: number;
  /** 每簇字节数（2 的幂，512..32768）。 */
  clusterSize?: number;
  timezoneOffsetMinutes?: number;
  dirs?: ExfatDirSpec[];
  files: ExfatFileSpec[];
  /** 未分配区域随机化。 */
  noiseBytes?: number;
}

export interface ExfatExpectations {
  files: Array<{
    path: string;
    name: string;
    deleted: boolean;
    size: number;
    sha256: string;
    md5: string;
    firstCluster: number;
    clusters: number[];
    chained: boolean;
    expectStatus: 'complete' | 'partial' | 'overwritten';
    overwrittenClusters: number;
    createdUtc: string;
    modifiedUtc: string;
    accessedUtc: string;
  }>;
  dirs: Array<{ path: string; deleted: boolean }>;
  volume: { totalClusters: number; clusterSize: number; label: string; freeClusters: number };
  imageSha256: string;
  imageMd5: string;
}

const FAT_EOC = 0xffffffff;

export function buildExfat(spec: ExfatCaseSpec): { bytes: Uint8Array; expectations: ExfatExpectations } {
  const clusterSize = spec.clusterSize ?? 4096;
  const sizeBytes = spec.sizeBytes ?? 32 * 1024 * 1024;
  const bytesPerSector = 512;
  const sectorsPerCluster = clusterSize / bytesPerSector;
  if (!Number.isInteger(sectorsPerCluster) || sectorsPerCluster < 1 || sectorsPerCluster > 32) {
    throw new Error('clusterSize 必须为 512..16384 的 2 的幂');
  }
  const sectorShift = 9;
  const clusterShift = Math.log2(sectorsPerCluster);
  const volumeSectors = Math.floor(sizeBytes / bytesPerSector);

  const reservedSectors = 24; // 主引导区 12 + 备份 12
  const fatOffsetSectors = reservedSectors;
  // FAT 大小：至少覆盖全部簇（4 字节/簇），并按扇区对齐。
  let fatLengthSectors = Math.ceil(((volumeSectors / sectorsPerCluster) + 2) * 4 / bytesPerSector);
  fatLengthSectors = Math.ceil(fatLengthSectors / sectorsPerCluster) * sectorsPerCluster;
  const clusterHeapSectors = volumeSectors - reservedSectors - fatLengthSectors;
  const clusterCount = Math.floor(clusterHeapSectors / sectorsPerCluster);
  if (clusterCount < 16) throw new Error('卷太小');

  const image = new Uint8Array(volumeSectors * bytesPerSector);
  const dataOffset = (reservedSectors + fatLengthSectors) * bytesPerSector;
  const fatOffset = fatOffsetSectors * bytesPerSector;
  const clusterOffset = (c: number): number => dataOffset + (c - 2) * clusterSize;

  const tzMinutes = spec.timezoneOffsetMinutes ?? 480;
  const label = spec.label ?? 'DASHEX';
  const serial = spec.serial ?? 0x5a5b5c5d;

  // 系统文件布局：簇 2 位图（1 簇）、簇 3.. 起 Up-case 表（128KiB）、随后根目录 4 簇。
  const bitmapClusters = 1;
  const upcaseBytes = 0x10000 * 2;
  const upcaseClusters = Math.ceil(upcaseBytes / clusterSize);
  const upcaseStart = 2 + bitmapClusters;
  const rootStart = upcaseStart + upcaseClusters;
  const rootClusterCount = 4;

  writeBootRegions(image, {
    bytesPerSector,
    sectorsPerCluster,
    sectorShift,
    clusterShift,
    fatOffsetSectors,
    fatLengthSectors,
    clusterHeapOffsetSectors: reservedSectors + fatLengthSectors,
    clusterCount,
    rootCluster: rootStart,
    serial,
    label,
    volumeSectors,
  });

  const fat = new Uint8Array(fatLengthSectors * bytesPerSector);
  const bitmap = new Uint8Array(Math.ceil(clusterCount / 8));
  const allocated = new Set<number>();

  const markAllocated = (c: number): void => {
    allocated.add(c);
    const bit = c - 2;
    bitmap[bit >> 3] = (bitmap[bit >> 3] ?? 0) | (1 << (bit & 7));
  };
  const setFat = (c: number, v: number): void => {
    fat[c * 4] = v & 0xff;
    fat[c * 4 + 1] = (v >>> 8) & 0xff;
    fat[c * 4 + 2] = (v >>> 16) & 0xff;
    fat[c * 4 + 3] = (v >>> 24) & 0xff;
  };

  // 固定分配：位图、Up-case 表、根目录
  for (let c = 2; c < rootStart + rootClusterCount; c++) markAllocated(c);
  setFat(2, FAT_EOC);
  for (let i = 0; i < upcaseClusters; i++) {
    setFat(upcaseStart + i, i + 1 < upcaseClusters ? upcaseStart + i + 1 : FAT_EOC);
  }
  for (let i = 0; i < rootClusterCount; i++) {
    setFat(rootStart + i, i + 1 < rootClusterCount ? rootStart + i + 1 : FAT_EOC);
  }
  writeUpcaseTable(image, clusterOffset(upcaseStart), upcaseBytes);
  const rootClusters: number[] = [];
  for (let i = 0; i < rootClusterCount; i++) rootClusters.push(rootStart + i);
  const rootBytes = new Uint8Array(rootClusters.length * clusterSize);
  // 根目录卷标项
  writeLabelEntry(rootBytes, 0, label);
  // 位图引用项（占用根目录槽位 1）
  writeBitmapEntry(rootBytes, 1, 2, bitmap.length);
  // Up-case 表引用项（槽位 2）
  writeUpcaseEntry(rootBytes, 2, upcaseStart, upcaseBytes);

  let nextFree = rootStart + rootClusterCount;
  const allocateContiguous = (count: number): number[] => {
    const out: number[] = [];
    for (let c = nextFree; c < clusterCount + 2 && out.length < count; c++) {
      if (!allocated.has(c)) {
        out.push(c);
        markAllocated(c);
      }
    }
    if (out.length < count) throw new Error('exFAT 空间不足');
    nextFree = out[out.length - 1]! + 1;
    return out;
  };
  const allocateChained = (count: number): number[] => {
    const out: number[] = [];
    for (let c = nextFree; c < clusterCount + 2 && out.length < count; c += 2) {
      if (!allocated.has(c)) {
        out.push(c);
        markAllocated(c);
      }
    }
    if (out.length < count) return allocateContiguous(count - out.length).reduce((a, c) => [...a, c], out);
    nextFree = Math.max(nextFree, out[out.length - 1]! + 1);
    return out;
  };

  const dirs = new Map<string, { clusters: number[]; bytes: Uint8Array }>();
  dirs.set('', { clusters: rootClusters, bytes: rootBytes });

  const pendingDirs: ExfatDirSpec[] = spec.dirs ?? [];
  for (const d of pendingDirs) {
    const parentPath = d.dir ?? '';
    const parent = dirs.get(parentPath);
    if (!parent) throw new Error(`父目录不存在: ${parentPath}`);
    const clusters = allocateContiguous(1);
    setFat(clusters[0]!, FAT_EOC);
    const bytes = new Uint8Array(clusterSize);
    const childPath = parentPath === '' ? d.name : `${parentPath}/${d.name}`;
    const slot = appendEntrySet(parent, {
      name: d.name,
      attributes: 0x10,
      firstCluster: clusters[0]!,
      // 目录：DataLength 记录目录簇链长度（fsck 期望非零），属性位标识目录。
      dataLength: clusterSize,
      noFatChain: false,
      created: d.created,
      modified: d.modified,
      accessed: d.accessed,
      deleted: d.deleted ?? false,
    });
    void slot;
    dirs.set(childPath, { clusters, bytes });
  }

  const pendingFiles: Array<{
    spec: ExfatFileSpec;
    clusters: number[];
    content: Uint8Array;
    dirPath: string;
  }> = [];

  for (const f of spec.files) {
    const dirPath = f.dir ?? '';
    if (!dirs.has(dirPath)) throw new Error(`文件目录不存在: ${dirPath}`);
    const content = f.content ?? pseudoRandom(f.size, f.seed ?? seedFromName(f.name));
    if (content.length !== f.size) throw new Error('content 长度不符');
    const count = Math.max(1, Math.ceil(f.size / clusterSize));
    const clusters = f.chained ? allocateChained(count) : allocateContiguous(count);
    if (f.chained) {
      for (let i = 0; i < clusters.length; i++) {
        setFat(clusters[i]!, i + 1 < clusters.length ? clusters[i + 1]! : FAT_EOC);
      }
    } else {
      for (const c of clusters) setFat(c, FAT_EOC);
    }
    pendingFiles.push({ spec: f, clusters, content, dirPath });
  }

  for (const pf of pendingFiles) {
    pf.clusters.forEach((c, i) => {
      const chunk = new Uint8Array(clusterSize);
      chunk.set(pf.content.subarray(i * clusterSize, Math.min((i + 1) * clusterSize, pf.content.length)));
      image.set(chunk, clusterOffset(c));
    });
  }

  const expectations: ExfatExpectations['files'] = [];
  for (const pf of pendingFiles) {
    const dir = dirs.get(pf.dirPath)!;
    appendEntrySet(dir, {
      name: pf.spec.name,
      attributes: 0x20,
      firstCluster: pf.clusters[0]!,
      dataLength: pf.spec.size,
      validDataLength: pf.spec.size,
      noFatChain: !pf.spec.chained,
      created: pf.spec.created,
      modified: pf.spec.modified,
      accessed: pf.spec.accessed,
      deleted: pf.spec.deleted ?? false,
    });
    const overwrittenClusters = pf.spec.overwriteBytesAfterDelete
      ? Math.ceil(pf.spec.overwriteBytesAfterDelete / clusterSize)
      : 0;
    if (pf.spec.deleted) {
      for (const c of pf.clusters) {
        const bit = c - 2;
        bitmap[bit >> 3] = (bitmap[bit >> 3] ?? 0) & ~(1 << (bit & 7));
        setFat(c, 0);
      }
      if (overwrittenClusters > 0) {
        const fill = pseudoRandom(overwrittenClusters * clusterSize, 0xbadc0de ^ (pf.spec.seed ?? 1));
        pf.clusters.slice(0, overwrittenClusters).forEach((c, i) => {
          image.set(fill.subarray(i * clusterSize, (i + 1) * clusterSize), clusterOffset(c));
          markAllocated(c);
        });
      }
    }
    const expectStatus: 'complete' | 'partial' | 'overwritten' =
      !pf.spec.deleted ? 'complete'
        : overwrittenClusters >= pf.clusters.length ? 'overwritten'
          : overwrittenClusters > 0 ? 'partial' : 'complete';
    expectations.push({
      path: pf.dirPath === '' ? `/${pf.spec.name}` : `/${pf.dirPath}/${pf.spec.name}`,
      name: pf.spec.name,
      deleted: pf.spec.deleted ?? false,
      size: pf.spec.size,
      sha256: sha256Hex(pf.content),
      md5: md5Hex(pf.content),
      firstCluster: pf.clusters[0]!,
      clusters: pf.clusters,
      chained: !!pf.spec.chained,
      expectStatus,
      overwrittenClusters,
      createdUtc: pf.spec.created.toISOString(),
      modifiedUtc: pf.spec.modified.toISOString(),
      accessedUtc: pf.spec.accessed.toISOString(),
    });
  }

  // 写入目录簇
  for (const d of dirs.values()) {
    d.clusters.forEach((c, i) => {
      image.set(d.bytes.subarray(i * clusterSize, (i + 1) * clusterSize), clusterOffset(c));
    });
  }

  // 位图写入簇 2
  image.set(bitmap, clusterOffset(2));
  // FAT 写入
  image.set(fat, fatOffset);

  // 噪声
  if (spec.noiseBytes && spec.noiseBytes > 0) {
    const start = clusterOffset(clusterCount + 2 - Math.ceil(spec.noiseBytes / clusterSize));
    let x = 0x9e3779b9;
    for (let i = 0; i < spec.noiseBytes && start + i < image.length; i++) {
      x ^= x << 13; x >>>= 0; x ^= x >>> 17; x ^= x << 5; x >>>= 0;
      if ((image[start + i] ?? 0) === 0) image[start + i] = x & 0xff;
    }
  }

  // FSInfo 百分比 & 引导区校验和
  let freeCount = 0;
  for (let c = 2; c < clusterCount + 2; c++) if (!allocated.has(c)) freeCount++;
  const percent = Math.min(100, Math.round(((clusterCount - freeCount) / clusterCount) * 100));
  image[112] = percent;
  finalizeBootChecksums(image, bytesPerSector);

  return {
    bytes: image,
    expectations: {
      files: expectations,
      dirs: [...dirs.keys()].filter((p) => p !== '').map((p) => ({ path: `/${p}`, deleted: false })),
      volume: { totalClusters: clusterCount, clusterSize, label, freeClusters: freeCount },
      imageSha256: sha256Hex(image),
      imageMd5: md5Hex(image),
    },
  };

  // ---------- 内部 ----------

  function appendEntrySet(
    dir: { clusters: number[]; bytes: Uint8Array },
    f: {
      name: string;
      attributes: number;
      firstCluster: number;
      dataLength: number;
      validDataLength?: number;
      noFatChain: boolean;
      created: Date;
      modified: Date;
      accessed: Date;
      deleted: boolean;
    },
  ): number {
    const nameUnits = [...f.name];
    const nameEntryCount = Math.max(1, Math.ceil(nameUnits.length / 15));
    const span = 2 + nameEntryCount;
    const slot = findSlot(dir, span);
    const byteAt = slot * 32;
    const set = new Uint8Array(span * 32);
    const inUse = f.deleted ? 0 : 0x80;

    // 0x85 文件目录项
    set[0] = inUse | 0x05;
    set[1] = span - 1;
    set[2] = 0;
    set[3] = 0;
    // SecondaryFlags 必须为 0（每个次级项另有自己的标志字节）。
    w16(set, 4, f.attributes);
    w16(set, 6, 0);
    const c = packExfatTime(f.created, tzMinutes);
    const m = packExfatTime(f.modified, tzMinutes);
    const a = packExfatTime(f.accessed, tzMinutes);
    // 权威布局（fs/exfat/exfat_raw.h struct exfat_dentry.file）：
    //   8/10 create time/date, 12/14 modify time/date, 16/18 access time/date,
    //   20 create_time_cs, 21 modify_time_cs, 22 create_tz, 23 modify_tz, 24 access_tz。
    w16(set, 8, c.time); w16(set, 10, c.date);
    w16(set, 12, m.time); w16(set, 14, m.date);
    w16(set, 16, a.time); w16(set, 18, a.date);
    set[20] = c.tenths; set[21] = m.tenths;
    set[22] = c.tz; set[23] = m.tz; set[24] = a.tz;

    // 0xC0 流扩展项
    const streamAt = 32;
    set[streamAt] = inUse | 0x40;
    // StreamFlags: bit0 AllocationPossible 必须为 1（已分配簇的文件/目录），
    // bit1 NoFatChain 表示连续分配。macOS 生成的卷统一为 0x03 / 0x01。
    set[streamAt + 1] = 0x01 | (f.noFatChain ? 0x02 : 0x00);
    set[streamAt + 2] = 0;
    set[streamAt + 3] = nameUnits.length;
    w16(set, streamAt + 4, nameHash(f.name));
    w16(set, streamAt + 6, 0);
    w64(set, streamAt + 8, f.validDataLength ?? f.dataLength);
    w32(set, streamAt + 20, f.firstCluster);
    w64(set, streamAt + 24, f.dataLength);

    // 0xC1 名称项
    for (let k = 0; k < nameEntryCount; k++) {
      const at = 32 * (2 + k);
      set[at] = inUse | 0x41;
      for (let i = 0; i < 15; i++) {
        const u = nameUnits[k * 15 + i];
        w16(set, at + 2 + i * 2, u === undefined ? 0 : u.codePointAt(0)!);
      }
    }
    // 条目集校验和
    const checksum = entrySetChecksum(set);
    w16(set, 2, checksum);

    dir.bytes.set(set, byteAt);
    return slot;
  }

  function findSlot(dir: { clusters: number[]; bytes: Uint8Array }, span: number): number {
    const capacity = dir.clusters.length * (clusterSize / 32);
    let used = 0;
    for (let i = 0; i < capacity; i++) {
      if (dir.bytes[i * 32] !== 0x00) used++;
    }
    if (used + span + 1 > capacity) {
      // 扩容：追加一簇并链接
      const extra = allocateContiguous(1);
      setFat(dir.clusters[dir.clusters.length - 1]!, extra[0]!);
      setFat(extra[0]!, FAT_EOC);
      const bigger = new Uint8Array(dir.bytes.length + clusterSize);
      bigger.set(dir.bytes);
      dir.bytes = bigger;
      dir.clusters.push(extra[0]!);
      return used;
    }
    return used;
  }
}

function writeBootRegions(
  image: Uint8Array,
  p: {
    bytesPerSector: number;
    sectorsPerCluster: number;
    sectorShift: number;
    clusterShift: number;
    fatOffsetSectors: number;
    fatLengthSectors: number;
    clusterHeapOffsetSectors: number;
    clusterCount: number;
    rootCluster: number;
    serial: number;
    label: string;
    volumeSectors: number;
  },
): void {
  const mkBoot = (): Uint8Array => {
    const b = new Uint8Array(512);
    b[0] = 0xeb; b[1] = 0x76; b[2] = 0x90;
    b.set(ascii('EXFAT   ', 8), 3);
    w64(b, 64, 0); // PartitionOffset
    w64(b, 72, p.volumeSectors);
    w32(b, 80, p.fatOffsetSectors);
    w32(b, 84, p.fatLengthSectors);
    w32(b, 88, p.clusterHeapOffsetSectors);
    w32(b, 92, p.clusterCount);
    w32(b, 96, p.rootCluster);
    w32(b, 100, p.serial);
    w16(b, 104, 0x0100);
    w16(b, 106, 0);
    b[108] = p.sectorShift;
    b[109] = p.clusterShift;
    b[110] = 1;
    b[111] = 1;
    b[112] = 0;
    b[510] = 0x55; b[511] = 0xaa;
    return b;
  };
  const boot = mkBoot();
  image.set(boot, 0);
  // 扩展引导扇区 1..8
  for (let s = 1; s <= 8; s++) {
    const ext = new Uint8Array(512);
    ext[510] = 0x55;
    ext[511] = 0xaa;
    image.set(ext, s * 512);
  }
  // OEM 参数区 9、10
  image.fill(0, 9 * 512, 10 * 512);
  // 校验和扇区 11：32 位校验和在整个扇区内重复（规范 3.2.3）。
  const checksum = bootChecksum(image, p.bytesPerSector);
  const csum = new Uint8Array(512);
  for (let off = 0; off < 512; off += 4) w32(csum, off, checksum);
  image.set(csum, 11 * 512);
  // 备份 12..23
  image.copyWithin(12 * 512, 0, 12 * 512);
}

function finalizeBootChecksums(image: Uint8Array, bytesPerSector: number): void {
  // 主引导区校验和只覆盖扇区 0..10，因此可以安全地重算。
  const checksum = bootChecksum(image, bytesPerSector);
  const csum = new Uint8Array(bytesPerSector);
  for (let off = 0; off < bytesPerSector; off += 4) w32(csum, off, checksum);
  image.set(csum, 11 * bytesPerSector);
  image.set(csum, 23 * bytesPerSector);
}

function bootChecksum(image: Uint8Array, bytesPerSector: number): number {
  let crc = 0;
  for (let i = 0; i < 11 * bytesPerSector; i++) {
    if (i === 106 || i === 107 || i === 112) continue;
    crc = ((crc >>> 1) | ((crc & 1) << 31)) >>> 0;
    crc = (crc + image[i]!) >>> 0;
  }
  return crc;
}

function writeUpcaseTable(image: Uint8Array, offset: number, length: number): void {
  // 完整 65536 项 Up-case 表：仅映射 ASCII 小写 → 大写，其余恒等。
  const entries = 0x10000;
  const table = new Uint8Array(entries * 2);
  for (let i = 0; i < entries; i++) {
    let v = i;
    if (i >= 0x61 && i <= 0x7a) v = i - 0x20;
    w16(table, i * 2, v);
  }
  image.set(table.subarray(0, length), offset);
}

/** Up-case 表校验和：与文件名哈希相同的 16 位循环右移累加。 */
export function upcaseChecksum(table: Uint8Array): number {
  let sum = 0;
  for (let i = 0; i < table.length; i++) {
    sum = (((sum & 1) << 15) + (sum >>> 1) + table[i]!) & 0xffff;
  }
  return sum;
}

function writeUpcaseEntry(dirBytes: Uint8Array, slot: number, firstCluster: number, dataLength: number): void {
  const at = slot * 32;
  dirBytes[at] = 0x82;
  // 校验和字段为表内容的 32 位求和；此处以同一算法计算 16 位版本会与 fsck 不符，
  // 故直接计算 32 位 CRC 型校验和（规范 7.2.5.2）。
  w32(dirBytes, at + 4, upcaseTableChecksum(dataLength));
  w32(dirBytes, at + 20, firstCluster);
  w64(dirBytes, at + 24, dataLength);
}

/** 生成器内部使用与表内容一致的校验和（表内容固定，可预计算）。 */
function upcaseTableChecksum(dataLength: number): number {
  const entries = 0x10000;
  const table = new Uint8Array(entries * 2);
  for (let i = 0; i < entries; i++) {
    let v = i;
    if (i >= 0x61 && i <= 0x7a) v = i - 0x20;
    w16(table, i * 2, v);
  }
  let crc = 0;
  const use = table.subarray(0, dataLength);
  for (let i = 0; i < use.length; i++) {
    crc = ((crc >>> 1) | ((crc & 1) << 31)) >>> 0;
    crc = (crc + use[i]!) >>> 0;
  }
  return crc;
}

function writeLabelEntry(dirBytes: Uint8Array, slot: number, label: string): void {
  const at = slot * 32;
  dirBytes[at] = 0x83;
  const units = [...label];
  dirBytes[at + 1] = units.length;
  for (let i = 0; i < Math.min(11, units.length); i++) {
    w16(dirBytes, at + 2 + i * 2, units[i]!.codePointAt(0)!);
  }
}

function writeBitmapEntry(dirBytes: Uint8Array, slot: number, firstCluster: number, dataLength: number): void {
  const at = slot * 32;
  dirBytes[at] = 0x81;
  // Flags: bit0 = 使用第一份位图。Bit 1 为 TexFAT 的第二份位图（此处不用）。
  dirBytes[at + 1] = 0x00;
  w32(dirBytes, at + 20, firstCluster);
  w64(dirBytes, at + 24, dataLength);
}

/**
 * exFAT 名称哈希（规范 7.5.2）：按 Up-case 表转大写后，每个 UTF-16 码元
 * 的两个字节各做一次 16 位循环右移累加。
 */
export function nameHash(name: string): number {
  let hash = 0;
  const upper = name.toUpperCase();
  for (let i = 0; i < upper.length; i++) {
    const cp = upper.charCodeAt(i);
    for (const byte of [cp & 0xff, cp >>> 8]) {
      hash = (((hash & 1) << 15) + (hash >>> 1) + byte) & 0xffff;
    }
  }
  return hash;
}

export function entrySetChecksum(set: Uint8Array): number {
  let sum = 0;
  for (let i = 0; i < set.length; i++) {
    if (i === 2 || i === 3) continue;
    sum = ((sum & 1) ? 0x8000 : 0) + (sum >>> 1) + set[i]!;
    sum &= 0xffff;
  }
  return sum;
}

function packExfatTime(d: Date, tzMinutes: number): { time: number; date: number; tenths: number; tz: number } {
  const local = new Date(d.getTime() + tzMinutes * 60_000);
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
  const units = Math.round(tzMinutes / 15);
  const tz = units < 0 ? 256 + units : units;
  return { time, date, tenths, tz };
}

export function decodeExfatTimeForTest(
  time: number,
  date: number,
  tenths: number,
  tz: number,
): string {
  const year = 1980 + ((date >>> 9) & 0x7f);
  const month = (date >>> 5) & 0x0f;
  const day = date & 0x1f;
  const hour = (time >>> 11) & 0x1f;
  const minute = (time >>> 5) & 0x3f;
  const second = (time & 0x1f) * 2 + Math.floor(tenths * 10 / 1000);
  const signed = tz > 127 ? tz - 256 : tz;
  const mins = signed * 15;
  const d = new Date(Date.UTC(year, month - 1, day, hour, minute, second));
  return new Date(d.getTime() - mins * 60_000).toISOString();
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
function w64(buf: Uint8Array, off: number, v: number): void {
  const lo = v % 0x1_0000_0000;
  const hi = Math.floor(v / 0x1_0000_0000);
  w32(buf, off, lo);
  w32(buf, off + 4, hi);
}
function ascii(s: string, len: number): Uint8Array {
  const out = new Uint8Array(len).fill(0x20);
  for (let i = 0; i < len && i < s.length; i++) out[i] = s.charCodeAt(i) & 0xff;
  return out;
}
function pseudoRandom(length: number, seed: number): Uint8Array {
  const out = new Uint8Array(length);
  let x = seed >>> 0 || 1;
  for (let i = 0; i < length; i++) {
    x ^= x << 13; x >>>= 0; x ^= x >>> 17; x ^= x << 5; x >>>= 0;
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
function sha256Hex(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex');
}
function md5Hex(data: Uint8Array): string {
  return createHash('md5').update(data).digest('hex');
}
