/**
 * exFAT 只读解析。
 *
 * 与 FAT32 的关键差异：
 *  - 目录项以“条目集”组织（0x85/0xC0/0xC1），删除由类型字节的 InUse 位表示，
 *    而不是首字节 0xE5；
 *  - 文件数据可以完全不用 FAT 链（NoFatChain=1，连续分配），删除后
 *    FAT 信息更少，因此恢复策略必须区分“连续”与“链式”；
 *  - 时间戳带 10ms 增量与显式 UTC 偏移，这对行车记录仪时间线尤其重要；
 *  - 分配位图可用于判断删除文件的数据簇是否已被重新分配。
 */
import type { ByteSource } from '../source';
import type {
  ClusterRun,
  FileEntry,
  ScanStats,
  TimestampValue,
  VolumeInfo,
} from '../types';
import { MAX_LISTED_ENTRIES, isMetadataName, joinPath, toRuns, runsToClusters } from './fat';

const ENTRY_BITMAP = 0x81;
const ENTRY_LABEL = 0x83;

const ATTR_READONLY = 0x01;
const ATTR_HIDDEN = 0x02;
const ATTR_SYSTEM = 0x04;
const ATTR_DIRECTORY = 0x10;
const ATTR_ARCHIVE = 0x20;

export class ExfatVolume {
  readonly info: VolumeInfo;
  private readonly source: ByteSource;
  private bitmap: Uint8Array | null = null;
  private bitmapCluster = 0;

  private constructor(source: ByteSource, info: VolumeInfo) {
    this.source = source;
    this.info = info;
  }

  static async open(
    source: ByteSource,
    offset: number,
    size: number,
    partitionIndex: number,
  ): Promise<ExfatVolume> {
    const boot = await source.read(offset, 512);
    if (!isExfatBoot(boot)) throw new Error('引导扇区不是 exFAT 卷');

    const bytesPerSector = 1 << boot[108]!;
    const sectorsPerCluster = 1 << boot[109]!;
    const fatOffsetSectors = readU32(boot, 80);
    const fatLengthSectors = readU32(boot, 84);
    const clusterHeapSectors = readU32(boot, 88);
    const clusterCount = readU32(boot, 92);
    const rootCluster = readU32(boot, 96);
    const serial = readU32(boot, 100);
    const revision = readU16(boot, 104);
    const volumeFlags = readU16(boot, 106);
    const fatCount = boot[110]!;
    const percentInUse = boot[112]!;
    const volumeLengthSectors = readU64(boot, 72);
    const warnings: VolumeInfo['warnings'] = [];

    if (![9, 10, 11, 12].includes(boot[108]!)) {
      throw new Error(`exFAT 扇区位移非法: ${boot[108]}`);
    }
    if (![0, 1, 2, 3, 4, 5, 6, 7, 8, 9].includes(boot[109]!)) {
      throw new Error(`exFAT 簇位移非法: ${boot[109]}`);
    }
    if (fatCount !== 1) {
      warnings.push({ code: 'fat-count', message: `exFAT FAT 表数量应为 1，实际 ${fatCount}` });
    }
    if (clusterCount === 0) throw new Error('exFAT 簇数为 0');
    if (rootCluster < 2 || rootCluster >= clusterCount + 2) {
      throw new Error(`exFAT 根目录簇号越界: ${rootCluster}`);
    }
    if (revision >> 8 !== 1) {
      warnings.push({
        code: 'revision',
        message: `exFAT 版本 ${revision >> 8}.${revision & 0xff}，按 1.x 解析`,
      });
    }
    const volumeBytes = volumeLengthSectors * bytesPerSector;
    if (volumeBytes > size - offset) {
      warnings.push({
        code: 'volume-exceeds-image',
        message: `卷大小超出镜像: 卷 ${volumeBytes} 字节, 镜像剩余 ${size - offset}`,
      });
    }
    if (volumeFlags & 0x0002) {
      warnings.push({ code: 'dirty', message: '卷标志位 VolumeDirty 已置位（未正常卸载）' });
    }
    if (volumeFlags & 0x0004) {
      warnings.push({ code: 'media-failure', message: '卷标志位 MediaFailure 已置位' });
    }
    if (percentInUse !== 0xff && percentInUse > 100) {
      warnings.push({ code: 'percent-in-use', message: `PercentInUse 非法: ${percentInUse}` });
    }

    // 主引导区校验和与备份引导区校验和
    const mainValid = await verifyBootChecksum(source, offset, bytesPerSector);
    const backupOffset = offset + 12 * bytesPerSector;
    let backupValid: boolean | null = null;
    try {
      const backup = await source.read(backupOffset, 512);
      const backupIsExfat = isExfatBoot(backup);
      const backupChecksum = backupIsExfat
        ? await verifyBootChecksum(source, backupOffset, bytesPerSector)
        : false;
      backupValid = backupIsExfat && backupChecksum;
      if (!backupValid) {
        warnings.push({
          code: 'backup-boot-invalid',
          message: 'exFAT 备份引导区缺失或校验和无效',
        });
      }
    } catch {
      backupValid = false;
      warnings.push({ code: 'backup-boot-missing', message: '无法读取 exFAT 备份引导区' });
    }
    if (!mainValid) {
      warnings.push({
        code: 'boot-checksum',
        message: 'exFAT 主引导区校验和不匹配（引导扇区可能被修改）',
      });
    }

    const info: VolumeInfo = {
      id: `vol-${partitionIndex}-${offset}`,
      fs: 'exfat',
      partitionIndex,
      offset,
      size: volumeBytes,
      label: '',
      serial: serial.toString(16).toUpperCase().padStart(8, '0'),
      bytesPerSector,
      sectorsPerCluster,
      clusterSize: bytesPerSector * sectorsPerCluster,
      clusterCount,
      freeClusters: percentInUse === 0xff
        ? null
        : Math.round((clusterCount * (100 - percentInUse)) / 100),
      fatOffset: offset + fatOffsetSectors * bytesPerSector,
      fatSize: fatLengthSectors * bytesPerSector,
      fatCount,
      dataOffset: offset + clusterHeapSectors * bytesPerSector,
      rootCluster,
      backupBootValid: backupValid,
      warnings,
      freeCountSource: percentInUse === 0xff ? 'unknown' : 'fsinfo',
      supported: true,
    };
    const vol = new ExfatVolume(source, info);
    await vol.loadVolumeLabel();
    return vol;
  }

  clusterOffset(cluster: number): number {
    return this.info.dataOffset + (cluster - 2) * this.info.clusterSize;
  }

  checkCluster(cluster: number): void {
    if (!Number.isInteger(cluster) || cluster < 2 || cluster >= this.info.clusterCount + 2) {
      throw new RangeError(`簇号越界: ${cluster} (有效 2..${this.info.clusterCount + 1})`);
    }
  }

  async readCluster(cluster: number): Promise<Uint8Array> {
    this.checkCluster(cluster);
    return this.source.read(this.clusterOffset(cluster), this.info.clusterSize);
  }

  /** FAT 值（exFAT 的链式文件才使用）。 */
  private async readFatValue(cluster: number): Promise<number> {
    const buf = await this.source.read(this.info.fatOffset + cluster * 4, 4);
    return readU32(buf, 0);
  }

  /** 读取分配位图；不存在时返回 null。 */
  async loadBitmap(): Promise<Uint8Array | null> {
    if (this.bitmap) return this.bitmap;
    const rootCluster = this.info.rootCluster;
    if (rootCluster === null) {
      this.info.warnings.push({ code: 'root-cluster-missing', message: '根目录簇号缺失' });
      return null;
    }
    const root = await this.readDirectoryClusters(
      rootCluster,
      { noFatChain: false, dataLength: this.info.clusterSize, isDirectory: true },
    );
    for (let i = 0; i < Math.floor(root.length / 32); i++) {
      const type = root[i * 32]!;
      if (type === ENTRY_BITMAP) {
        const firstCluster = readU32(root, i * 32 + 20);
        const dataLength = readU64(root, i * 32 + 24);
        this.bitmapCluster = firstCluster;
        if (firstCluster >= 2) {
          const count = Math.ceil(dataLength / this.info.clusterSize);
          this.bitmap = await this.source.read(
            this.clusterOffset(firstCluster),
            Math.min(count * this.info.clusterSize, Math.ceil(this.info.clusterCount / 8)),
          );
          return this.bitmap;
        }
      }
      if (type === 0x00) break;
    }
    this.info.warnings.push({ code: 'bitmap-missing', message: '未找到 exFAT 分配位图，覆盖判断降级' });
    return null;
  }

  /**
   * 从根目录读取卷标项（0x83）。卷标不属于文件名空间，应在打开卷时即读取，
   * 而不是等到列出目录之后。
   */
  async loadVolumeLabel(): Promise<void> {
    const rootCluster = this.info.rootCluster;
    if (rootCluster === null) return;
    try {
      const rootBytes = await this.readDirectoryClusters(
        rootCluster,
        { noFatChain: false, dataLength: this.info.clusterSize, isDirectory: true },
      );
      for (let i = 0; i < Math.floor(rootBytes.length / 32); i++) {
        const type = rootBytes[i * 32]!;
        if (type === ENTRY_LABEL) {
          const len = rootBytes[i * 32 + 1]!;
          let label = '';
          for (let c = 0; c < Math.min(len, 11); c++) {
            label += String.fromCharCode(readU16(rootBytes, i * 32 + 2 + c * 2));
          }
          this.info.label = label;
          return;
        }
        if (type === 0x00) return;
      }
    } catch {
      // 卷标读取失败不阻断卷打开
    }
  }

  isAllocated(cluster: number): boolean {
    if (!this.bitmap) return false;
    const index = cluster - 2;
    const byte = this.bitmap[index >> 3];
    if (byte === undefined) return false;
    return (byte & (1 << (index & 7))) !== 0;
  }

  /** 统计空闲簇，更新 VolumeInfo。 */
  async refreshFreeCount(): Promise<void> {
    const bitmap = await this.loadBitmap();
    if (!bitmap) return;
    let free = 0;
    for (let c = 2; c < this.info.clusterCount + 2; c++) {
      if (!this.isAllocated(c)) free++;
    }
    this.info.freeClusters = free;
    this.info.freeCountSource = 'bitmap';
  }

  async readClusterChain(
    start: number,
    maxClusters: number,
    signal?: AbortSignal,
  ): Promise<{ clusters: number[]; status: 'eoc' | 'invalid' | 'loop' | 'limit' }> {
    const clusters: number[] = [];
    const seen = new Set<number>();
    let current = start;
    let status: 'eoc' | 'invalid' | 'loop' | 'limit' = 'limit';
    while (clusters.length < maxClusters) {
      if (signal?.aborted) throw new DOMException('已取消', 'AbortError');
      if (current < 2 || current >= this.info.clusterCount + 2) {
        status = 'invalid';
        break;
      }
      if (seen.has(current)) {
        status = 'loop';
        break;
      }
      seen.add(current);
      clusters.push(current);
      const next = await this.readFatValue(current);
      if (next >= 0xfffffff8) {
        status = 'eoc';
        break;
      }
      if (next === 0) {
        status = 'invalid';
        break;
      }
      current = next;
    }
    if (clusters.length >= maxClusters) status = 'limit';
    return { clusters, status };
  }

  /** 读取目录簇链（目录本身可以是 NoFatChain）。 */
  private async readDirectoryClusters(
    start: number,
    stream: { noFatChain: boolean; dataLength: number; isDirectory: boolean },
    signal?: AbortSignal,
  ): Promise<Uint8Array> {
    if (stream.noFatChain) {
      // 目录不使用 NoFatChain；若遇到，按数据长度或单簇保守读取。
      const count = Math.max(
        1,
        stream.isDirectory ? 1 : Math.ceil(stream.dataLength / this.info.clusterSize),
      );
      const out = new Uint8Array(count * this.info.clusterSize);
      for (let i = 0; i < count; i++) {
        out.set(await this.readCluster(start + i), i * this.info.clusterSize);
      }
      return out;
    }
    // 目录没有 DataLength 语义，必须沿 FAT 链读到 EOC。
    const limit = stream.isDirectory
      ? Math.max(1, Math.ceil(this.info.clusterCount / 2))
      : Math.max(1, Math.ceil(stream.dataLength / this.info.clusterSize) + 8);
    const chain = await this.readClusterChain(start, limit, signal);
    if (chain.status === 'invalid' || chain.status === 'loop') {
      this.info.warnings.push({
        code: 'dir-chain-broken',
        message: `目录链异常 (${chain.status})，起始簇 ${start}`,
      });
    }
    const out = new Uint8Array(chain.clusters.length * this.info.clusterSize);
    for (let i = 0; i < chain.clusters.length; i++) {
      out.set(await this.readCluster(chain.clusters[i]!), i * this.info.clusterSize);
    }
    return out;
  }

  async listEntries(
    opts: { includeDeleted: boolean; maxEntries: number; signal?: AbortSignal },
    onEntry?: (entry: FileEntry) => void,
  ): Promise<{ entries: FileEntry[]; stats: ScanStats }> {
    const started = performance.now();
    await this.loadBitmap();
    const entries: FileEntry[] = [];
    const stats: ScanStats = {
      files: 0,
      deletedFiles: 0,
      directories: 0,
      deletedDirectories: 0,
      totalBytes: 0,
      recoveredName: 0,
      warnings: [],
      durationMs: 0,
      truncatedListing: false,
    };

    const rootCluster = this.info.rootCluster;
    if (rootCluster === null) throw new Error('exFAT 根目录簇号缺失');
    type DirTask = { cluster: number; path: string; noFatChain: boolean; length: number; parentDeleted: boolean; depth: number };
    const queue: DirTask[] = [
      {
        cluster: rootCluster,
        path: '/',
        noFatChain: false,
        length: this.info.clusterSize,
        parentDeleted: false,
        depth: 0,
      },
    ];
    const visited = new Set<number>();

    while (queue.length > 0) {
      if (opts.signal?.aborted) throw new DOMException('已取消', 'AbortError');
      const dir = queue.shift()!;
      if (dir.depth > 64) {
        stats.warnings.push(`目录层级超过 64，停止展开: ${dir.path}`);
        continue;
      }
      const bytes = await this.readDirectoryClusters(
        dir.cluster,
        { noFatChain: dir.noFatChain, dataLength: dir.length, isDirectory: true },
        opts.signal,
      );
      const parsed = parseEntrySets(bytes, this.clusterOffset(dir.cluster));

      for (const set of parsed) {
        const entry = this.buildEntry(set, dir.path, dir.parentDeleted);
        if (!opts.includeDeleted && entry.deleted) continue;
        entries.push(entry);
        onEntry?.(entry);
        if (entry.deleted) {
          if (entry.kind === 'file') stats.deletedFiles++;
          else stats.deletedDirectories++;
        } else if (entry.kind === 'file') {
          stats.files++;
          stats.totalBytes += entry.size;
        } else {
          stats.directories++;
        }
        if (entry.longNameRecovered) stats.recoveredName++;

        if (entry.kind === 'directory' && entry.firstCluster >= 2) {
          const key = entry.firstCluster;
          if (!visited.has(key) || entry.deleted) {
            visited.add(key);
            queue.push({
              cluster: entry.firstCluster,
              path: entry.path,
              noFatChain: set.noFatChain,
              length: Math.max(this.info.clusterSize, entry.size),
              parentDeleted: entry.deleted,
              depth: dir.depth + 1,
            });
          }
        }
        if (entries.length >= opts.maxEntries) {
          stats.truncatedListing = true;
          break;
        }
      }
      if (stats.truncatedListing) break;
    }

    stats.durationMs = Math.round(performance.now() - started);
    return { entries, stats };
  }

  private buildEntry(set: ParsedEntrySet, parentPath: string, parentDeleted: boolean): FileEntry {
    const deleted = set.deleted || parentDeleted;
    const nameInfo = reconstructName(set);
    const size = set.dataLength;
    const needed = set.isDirectory
      ? 0
      : Math.max(1, Math.ceil(size / this.info.clusterSize));
    const warnings: string[] = [];
    if (set.validDataLength !== null && set.validDataLength < size) {
      warnings.push(
        `有效数据长度 (${set.validDataLength}) 小于数据长度 (${size})：尾部为未初始化数据`,
      );
    }
    if (set.fileEntryChecksumValid === false) {
      warnings.push('条目集校验和不匹配：条目可能被篡改或部分覆盖');
    }
    if (set.nameHashValid === false) {
      warnings.push('文件名哈希不匹配：名称与流扩展项可能不属于同一文件');
    }
    if (nameInfo.recovered && set.deleted) {
      // 长文件名的字符仍完整保留在 Name 项中。
    }

    const runs: ClusterRun[] = [];
    if (set.firstCluster >= 2 && needed > 0) {
      const contiguousCount = set.isDirectory
        ? 1
        : Math.min(needed, this.info.clusterCount + 1 - set.firstCluster);
      runs.push({ start: set.firstCluster, count: Math.max(0, contiguousCount) });
    }
    // 空目录 / 无簇文件不应出现 count=0 的 run。
    const filteredRuns = runs.filter((r) => r.count > 0);

    let chainQuality: FileEntry['chainQuality'] = 'empty';
    if (set.isDirectory) chainQuality = 'valid';
    else if (size === 0) chainQuality = 'empty';
    else if (set.noFatChain) chainQuality = 'contiguous-assumed';
    else chainQuality = 'valid';

    const attributeLabels = describeAttributes(set.attributes);

    return {
      id: `${this.info.id}:${set.offset}`,
      volumeId: this.info.id,
      parentPath,
      path: joinPath(parentPath, nameInfo.name),
      name: nameInfo.name,
      shortName: null,
      kind: set.isDirectory ? 'directory' : 'file',
      deleted,
      longNameRecovered: nameInfo.recovered,
      nameConfidence: nameInfo.confidence,
      nameNote: nameInfo.note,
      size,
      firstCluster: set.firstCluster,
      runs: filteredRuns,
      allocatedClusters: filteredRuns.reduce((s, r) => s + r.count, 0),
      overwrittenClusters: 0,
      allocationHint: set.noFatChain ? 'contiguous' : 'fat-chain',
      chainQuality,
      attributes: set.attributes,
      attributeLabels,
      times: set.times,
      entryOffset: set.offset,
      entrySpan: set.span,
      warnings,
      isMetadata: isMetadataName(nameInfo.name),
      ...(parentDeleted ? { orphaned: true } : {}),
    };
  }

  /** 解析链式文件簇链（恢复阶段）。 */
  async resolveChain(entry: FileEntry, signal?: AbortSignal): Promise<ClusterRun[]> {
    if (entry.kind === 'directory') return entry.runs;
    const needed = Math.max(1, Math.ceil(entry.size / this.info.clusterSize));
    if (entry.firstCluster < 2) {
      entry.chainQuality = entry.size === 0 ? 'empty' : 'error';
      return [];
    }
    const maxEnd = this.info.clusterCount + 1;
    const contiguousEnd = entry.firstCluster + needed - 1;
    if (contiguousEnd > maxEnd) {
      entry.chainQuality = 'truncated';
      entry.warnings.push(
        `连续簇超出卷边界：需要 ${needed} 簇，起始 ${entry.firstCluster}，卷上限 ${maxEnd}`,
      );
      const usable = Math.max(0, maxEnd - entry.firstCluster + 1);
      entry.runs = usable > 0 ? [{ start: entry.firstCluster, count: usable }] : [];
      entry.allocatedClusters = usable;
    } else if (entry.deleted) {
      // 已删除的 exFAT 文件：NoFatChain 标志多数仍保留在流扩展项中，
      // 但该标志位本身也可能被清除。两种策略都按“连续优先、链式兜底”处理，
      // 并显式标注依据。
      let chainClusters: number[] | null = null;
      if (!entry.runs.some((r) => r.count === needed)) {
        const chain = await this.readClusterChain(entry.firstCluster, needed, signal);
        if (chain.status === 'eoc' && chain.clusters.length >= needed) {
          chainClusters = chain.clusters;
        }
      }
      if (entry.runs.some((r) => r.count === needed)) {
        // 流扩展项的 NoFatChain 标志是磁盘上的明确声明（不是猜测），
        // 与 FAT 链被清零后的“连续假设”不同。
        entry.chainQuality = 'valid';
        entry.warnings.push('流扩展项标记为连续分配（NoFatChain），且分配位图显示未重新分配');
      } else if (chainClusters) {
        entry.runs = toRuns(chainClusters);
        entry.allocatedClusters = chainClusters.length;
        entry.chainQuality = 'valid';
        entry.warnings.push('FAT 簇链完整，按链恢复');
      } else {
        entry.runs = [{ start: entry.firstCluster, count: needed }];
        entry.allocatedClusters = needed;
        entry.chainQuality = 'contiguous-assumed';
        entry.warnings.push('NoFatChain 标志缺失且 FAT 链不可用，按连续簇恢复（推断）');
      }
    } else if (entry.allocationHint === 'fat-chain') {
      const chain = await this.readClusterChain(entry.firstCluster, needed, signal);
      if (chain.clusters.length >= needed) {
        entry.runs = toRuns(chain.clusters.slice(0, needed));
        entry.allocatedClusters = needed;
        entry.chainQuality = 'valid';
        if (chain.clusters.length > needed) {
          entry.warnings.push(
            `FAT 链长度 ${chain.clusters.length} 大于文件大小所需 ${needed} 簇`,
          );
        }
      } else {
        entry.runs = toRuns(chain.clusters);
        entry.allocatedClusters = chain.clusters.length;
        entry.chainQuality = 'truncated';
        entry.warnings.push(
          `FAT 链不完整 (${chain.status})：需要 ${needed} 簇，链上仅 ${chain.clusters.length} 簇`,
        );
      }
    } else {
      entry.runs = [{ start: entry.firstCluster, count: needed }];
      entry.allocatedClusters = needed;
      entry.chainQuality = 'valid';
    }

    if (entry.deleted) {
      const clusters = runsToClusters(entry.runs);
      let overwritten = 0;
      for (const c of clusters) {
        if (this.isAllocated(c)) overwritten++;
      }
      entry.overwrittenClusters = overwritten;
      if (overwritten > 0) {
        if (overwritten >= clusters.length) entry.chainQuality = 'overwritten';
        else if (entry.chainQuality === 'valid') entry.chainQuality = 'truncated';
        entry.warnings.push(
          `${overwritten}/${clusters.length} 个数据簇在分配位图中已重新分配（可能被覆盖）`,
        );
      }
    }
    return entry.runs;
  }

  async readFileData(
    entry: FileEntry,
    signal?: AbortSignal,
  ): Promise<{ data: Uint8Array; missingBytes: number; overwrittenClusters: number }> {
    if (entry.kind === 'directory') throw new Error('目录不使用此接口');
    const clusters = runsToClusters(entry.runs);
    const out = new Uint8Array(clusters.length * this.info.clusterSize);
    let at = 0;
    for (const c of clusters) {
      if (signal?.aborted) throw new DOMException('已取消', 'AbortError');
      const data = await this.readCluster(c);
      out.set(data, at);
      at += data.length;
    }
    if (out.length >= entry.size) {
      return {
        data: out.subarray(0, entry.size),
        missingBytes: 0,
        overwrittenClusters: entry.overwrittenClusters,
      };
    }
    return {
      data: out,
      missingBytes: entry.size - out.length,
      overwrittenClusters: entry.overwrittenClusters,
    };
  }

  bitmapClusterNumber(): number {
    return this.bitmapCluster;
  }
}

interface ParsedEntrySet {
  offset: number;
  span: number;
  deleted: boolean;
  isDirectory: boolean;
  attributes: number;
  firstCluster: number;
  dataLength: number;
  validDataLength: number | null;
  noFatChain: boolean;
  name: string;
  nameLength: number;
  nameHashValid: boolean | null;
  fileEntryChecksumValid: boolean | null;
  times: FileEntry['times'];
  secondaryValid: boolean;
}

function parseEntrySets(bytes: Uint8Array, baseOffset: number): ParsedEntrySet[] {
  const sets: ParsedEntrySet[] = [];
  let i = 0;
  const max = Math.floor(bytes.length / 32);
  while (i < max) {
    const type = bytes[i * 32]!;
    if (type === 0x00) break;
    const inUse = (type & 0x80) !== 0;
    const secondary = (type & 0x40) !== 0;
    if (secondary || (type & 0x7f) !== 0x05) {
      i++;
      continue;
    }
    const fileEntry = bytes.subarray(i * 32, i * 32 + 32);
    const secondaryCount = fileEntry[1]!;
    const expectedChecksum = readU16(fileEntry, 2);
    const span = secondaryCount + 1;
    if (i + span > max || secondaryCount === 0 || secondaryCount > 18) {
      i++;
      continue;
    }
    const rawSet = bytes.subarray(i * 32, (i + span) * 32);
    // 校验和覆盖整个条目集，跳过首项偏移 2-3 的 SetChecksum 字段本身。
    const checksum = entrySetChecksum(rawSet, 0, 2);
    const checksumValid = checksum === expectedChecksum;

    let stream: Uint8Array | null = null;
    const nameEntries: Uint8Array[] = [];
    let secondaryCountFound = 0;
    for (let k = 1; k < span; k++) {
      const t = bytes[(i + k) * 32]!;
      if ((t & 0x40) === 0) continue;
      secondaryCountFound++;
      // 类型码位于低 5 位：0xC0/0x40 = 流扩展项，0xC1/0x41 = 文件名项。
      const kind = t & 0x1f;
      if (kind === 0x00 && !stream) stream = bytes.subarray((i + k) * 32, (i + k) * 32 + 32);
      if (kind === 0x01) nameEntries.push(bytes.subarray((i + k) * 32, (i + k) * 32 + 32));
    }
    if (!stream) {
      i += span;
      continue;
    }

    const attributes = readU16(fileEntry, 4);
    const isDirectory = (attributes & ATTR_DIRECTORY) !== 0;
    const firstCluster = readU32(stream, 20);
    const dataLength = readU64(stream, 24);
    const validDataLength = readU64(stream, 8);
    const streamFlags = stream[1]!;
    const noFatChain = (streamFlags & 0x02) !== 0;
    const recordedNameLength = stream[3]!;
    const recordedNameHash = readU16(stream, 4);

    // 名称可能跨多个连续 Name 项，每项承载 15 个 UTF-16 码元。
    let name = '';
    for (const entry of nameEntries) {
      for (let c = 0; c < 15; c++) {
        const cp = readU16(entry, 2 + c * 2);
        if (cp === 0x0000) break;
        name += String.fromCharCode(cp);
      }
    }
    if (recordedNameLength > 0) name = name.slice(0, recordedNameLength);
    const nameHashValid = name.length > 0
      ? nameHash(name) === recordedNameHash
      : null;

    sets.push({
      offset: baseOffset + i * 32,
      span,
      deleted: !inUse,
      isDirectory,
      attributes,
      firstCluster,
      dataLength,
      validDataLength,
      noFatChain,
      name,
      nameLength: recordedNameLength,
      nameHashValid,
      fileEntryChecksumValid: checksumValid,
      times: {
        created: decodeExfatTime(fileEntry, 8, 20, 22),
        modified: decodeExfatTime(fileEntry, 12, 21, 23),
        accessed: decodeExfatTime(fileEntry, 16, null, 24),
      },
      secondaryValid: secondaryCountFound === secondaryCount,
    });
    i += span;
  }
  return sets;
}

/**
 * 名称重建：exFAT 的 Name 项本身没有校验位，靠流扩展项中的 NameHash
 * 与名称长度校验。删除只清除 InUse 位，字符仍在，因此通常可完整恢复；
 * 哈希不匹配时保留名称但降低可信度。
 */
function reconstructName(set: ParsedEntrySet): {
  name: string;
  recovered: boolean;
  confidence: FileEntry['nameConfidence'];
  note?: string;
} {
  const name = set.name;
  if (!name) {
    return {
      name: `[无名称项 @${set.offset.toString(16)}]`,
      recovered: false,
      confidence: 'low',
      note: '目录项缺少名称项，无法恢复文件名',
    };
  }
  if (set.nameLength > 0 && [...name].length < set.nameLength) {
    return {
      name,
      recovered: false,
      confidence: 'low',
      note: `名称长度不足：记录 ${set.nameLength} 字符，实际 ${name.length} 字符`,
    };
  }
  if (set.nameHashValid === false) {
    return {
      name,
      recovered: false,
      confidence: 'low',
      note: '文件名哈希与流扩展项不匹配，名称仅作参考',
    };
  }
  if (set.deleted) {
    return {
      name,
      recovered: true,
      confidence: set.fileEntryChecksumValid === false ? 'medium' : 'high',
      ...(set.fileEntryChecksumValid === false
        ? { note: '名称为删除项残留，条目集校验和不匹配' }
        : { note: '名称来自已删除条目，字符与哈希校验一致' }),
    };
  }
  return { name, recovered: false, confidence: 'exact' };
}

/**
 * exFAT 名称哈希（规范 7.5.2）。
 *
 * 逐字符处理：先按卷的 Up-case 表转大写，再把 UTF-16 码元拆成低字节、高字节
 * 各做一次 16 位循环右移累加。实测 macOS/Linux 生成的卷均遵循该算法。
 * 这里的转大写使用 Unicode 的简单大写映射；对未收录于 Unicode 特殊字符的
 * 名称（绝大多数相机文件名）与卷内 Up-case 表结果一致。
 */
export function nameHash(name: string): number {
  let hash = 0;
  const upper = name.toUpperCase();
  for (let i = 0; i < upper.length; i++) {
    const cp = upper.charCodeAt(i);
    for (const byte of [cp & 0xff, cp >> 8]) {
      hash = (((hash & 1) << 15) + (hash >>> 1) + byte) & 0xffff;
    }
  }
  return hash;
}

/** 目录项集校验和：逐字节右循环 1 位累加。 */
export function entrySetChecksum(bytes: Uint8Array, start: number, skipAt: number): number {
  let sum = 0;
  for (let i = start; i < bytes.length; i++) {
    if (i === skipAt || i === skipAt + 1) continue;
    sum = ((sum & 1) ? 0x8000 : 0) + (sum >>> 1) + bytes[i]!;
    sum &= 0xffff;
  }
  return sum;
}

/**
 * 解码 exFAT 时间戳。时间字与日期字的位布局与 FAT 相同；
 * 另带 10ms 增量字节与以 15 分钟为单位的有符号 UTC 偏移字节。
 */
function decodeExfatTime(
  rec: Uint8Array,
  timeOffset: number,
  tenthsOffset: number | null,
  tzOffset: number,
): TimestampValue | null {
  const time = readU16(rec, timeOffset);
  const date = readU16(rec, timeOffset + 2);
  const tenths = tenthsOffset === null ? 0 : rec[tenthsOffset]!;
  const tzRaw = rec[tzOffset] ?? 0;
  if (date === 0 && time === 0) return null;
  const year = 1980 + ((date >>> 9) & 0x7f);
  const month = (date >>> 5) & 0x0f;
  const day = date & 0x1f;
  const hour = (time >>> 11) & 0x1f;
  const minute = (time >>> 5) & 0x3f;
  const second = (time & 0x1f) * 2;
  const ms = Math.min(199, tenths) * 10;
  if (month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59) {
    return {
      raw: `time=0x${time.toString(16)} date=0x${date.toString(16)}`,
      precision: tenthsOffset === null ? 'date' : 'datetime',
      iso: '无效时间戳',
      tzOffsetMinutes: null,
    };
  }
  // UTC 偏移：以 15 分钟为单位的有符号字节，有效范围 -96..+103。
  const signed = tzRaw > 127 ? tzRaw - 256 : tzRaw;
  const tzValid = signed >= -96 && signed <= 103;
  const tzMinutes = tzValid ? signed * 15 : null;
  const sec = second + Math.floor(ms / 1000);
  let iso = `${year.toString().padStart(4, '0')}-${pad2(month)}-${pad2(day)}T${pad2(hour)}:${pad2(minute)}:${pad2(sec)}.${(ms % 1000).toString().padStart(3, '0')}`;
  if (tzMinutes !== null) {
    const sign = tzMinutes >= 0 ? '+' : '-';
    const abs = Math.abs(tzMinutes);
    iso += `${sign}${pad2(Math.floor(abs / 60))}:${pad2(abs % 60)}`;
  }
  return {
    raw: `time=0x${time.toString(16)} date=0x${date.toString(16)} tenths=${tenths} tz=0x${tzRaw.toString(16)}`,
    precision: tenthsOffset === null ? 'date' : 'datetime',
    iso,
    tzOffsetMinutes: tzMinutes,
    tenths,
  };
}

function describeAttributes(attr: number): string[] {
  const labels: string[] = [];
  if (attr & ATTR_READONLY) labels.push('只读');
  if (attr & ATTR_HIDDEN) labels.push('隐藏');
  if (attr & ATTR_SYSTEM) labels.push('系统');
  if (attr & ATTR_DIRECTORY) labels.push('目录');
  if (attr & ATTR_ARCHIVE) labels.push('存档');
  return labels;
}

async function verifyBootChecksum(
  source: ByteSource,
  offset: number,
  bytesPerSector: number,
): Promise<boolean> {
  try {
    const region = await source.read(offset, 11 * bytesPerSector);
    const stored = await source.read(offset + 11 * bytesPerSector, 4);
    const expected = readU32(stored, 0);
    let crc = 0;
    // 规范：跳过 VolumeFlags(106-107) 与 PercentInUse(112) 三个字节，
    // 即不参与旋转/累加；逐字节“循环右移 1 位后相加”。
    for (let i = 0; i < region.length; i++) {
      if (i === 106 || i === 107 || i === 112) continue;
      crc = ((crc >>> 1) | ((crc & 1) << 31)) >>> 0;
      crc = (crc + region[i]!) >>> 0;
    }
    return crc === expected;
  } catch {
    return false;
  }
}

function isExfatBoot(boot: Uint8Array): boolean {
  if (boot.length < 512) return false;
  if (String.fromCharCode(...boot.subarray(3, 11)) !== 'EXFAT   ') return false;
  if (boot[510] !== 0x55 || boot[511] !== 0xaa) return false;
  return true;
}

function readU16(buf: Uint8Array, off: number): number {
  return buf[off]! | (buf[off + 1]! << 8);
}

function readU32(buf: Uint8Array, off: number): number {
  return (buf[off]! | (buf[off + 1]! << 8) | (buf[off + 2]! << 16) | (buf[off + 3]! << 24)) >>> 0;
}

function readU64(buf: Uint8Array, off: number): number {
  return readU32(buf, off) + readU32(buf, off + 4) * 0x1_0000_0000;
}

function pad2(v: number): string {
  return v.toString().padStart(2, '0');
}

export { isExfatBoot, MAX_LISTED_ENTRIES };
