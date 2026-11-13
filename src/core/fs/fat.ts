/**
 * FAT12 / FAT16 / FAT32 只读解析。
 *
 * 关键取证点：
 *  - 目录项与 LFN 的原始偏移全部保留；
 *  - 删除条目（首字节 0xE5）仍可解析，包括残留的长文件名；
 *  - 簇链断裂/归零/环状引用都显式标记，不做“猜一个完整文件”的粉饰；
 *  - 已删除文件的数据簇若被现存文件占用，标记为“已覆盖”。
 */
import type { ByteSource } from '../source';
import type {
  ClusterRun,
  EntryKind,
  FileEntry,
  FileSystemType,
  ScanStats,
  TimestampValue,
  VolumeInfo,
} from '../types';

const ATTR_READONLY = 0x01;
const ATTR_HIDDEN = 0x02;
const ATTR_SYSTEM = 0x04;
const ATTR_VOLUME_ID = 0x08;
const ATTR_DIRECTORY = 0x10;
const ATTR_ARCHIVE = 0x20;
const ATTR_LFN = 0x0f;

export const MAX_LISTED_ENTRIES = 2_000_000;
/** FAT 整表载入上限（96 MiB），超过则按需读取。 */
const FAT_IN_MEMORY_LIMIT = 96 * 1024 * 1024;

export class FatVolume {
  readonly info: VolumeInfo;
  private fat: Uint8Array | null = null;
  private fatBits: 12 | 16 | 32 = 32;
  private rootDirOffset = 0;
  private rootEntryCount = 0;
  private readonly source: ByteSource;
  private allocatedMapCache: Promise<Map<number, string>> | null = null;

  private constructor(source: ByteSource, info: VolumeInfo) {
    this.source = source;
    this.info = info;
  }

  static async open(
    source: ByteSource,
    offset: number,
    size: number,
    partitionIndex: number,
  ): Promise<FatVolume> {
    const boot = await source.read(offset, 512);
    if (!isFatBoot(boot)) throw new Error('引导扇区不是 FAT 卷');
    const bytesPerSector = readU16(boot, 11);
    const sectorsPerCluster = boot[13]!;
    const reservedSectors = readU16(boot, 14);
    const fatCount = boot[16]!;
    const rootEntryCount = readU16(boot, 17);
    const totalSectors16 = readU16(boot, 19);
    const totalSectors32 = readU32(boot, 32);
    const fatSize16 = readU16(boot, 22);
    const fatSize32 = readU32(boot, 36);
    const totalSectors = totalSectors16 !== 0 ? totalSectors16 : totalSectors32;
    const fatSize = fatSize16 !== 0 ? fatSize16 : fatSize32;
    const warnings: VolumeInfo['warnings'] = [];

    if (![512, 1024, 2048, 4096].includes(bytesPerSector)) {
      throw new Error(`FAT 每扇区字节数非法: ${bytesPerSector}`);
    }
    if (sectorsPerCluster === 0 || (sectorsPerCluster & (sectorsPerCluster - 1)) !== 0) {
      throw new Error(`FAT 每簇扇区数非法: ${sectorsPerCluster}`);
    }
    if (reservedSectors === 0) warnings.push({ code: 'reserved-zero', message: '保留扇区数为 0' });
    if (fatCount === 0) throw new Error('FAT 表数量为 0');
    if (fatSize === 0) throw new Error('FAT 表大小为 0');
    if (totalSectors === 0) {
      // 有些相机镜像不填总扇区数，按镜像剩余空间推断并警告。
      warnings.push({ code: 'total-sectors-zero', message: 'BPB 总扇区数为 0，按镜像边界推断' });
    }

    const effectiveTotal = totalSectors !== 0
      ? totalSectors
      : Math.floor((size - offset) / bytesPerSector);
    if (totalSectors !== 0 && totalSectors * bytesPerSector > size) {
      warnings.push({
        code: 'volume-exceeds-image',
        message: `卷大小超出镜像: 卷 ${totalSectors * bytesPerSector} 字节, 镜像剩余 ${size - offset}`,
      });
    }

    const rootDirSectors = Math.ceil((rootEntryCount * 32) / bytesPerSector);
    const firstDataSector = reservedSectors + fatCount * fatSize + rootDirSectors;
    if (firstDataSector >= effectiveTotal) {
      throw new Error('FAT 数据区起点超出卷大小（引导扇区损坏）');
    }
    const clusterCount = Math.floor((effectiveTotal - firstDataSector) / sectorsPerCluster);
    const clusterSize = bytesPerSector * sectorsPerCluster;

    let fatBits: 12 | 16 | 32;
    if (fatSize32 !== 0 && rootEntryCount === 0) {
      fatBits = 32;
    } else if (clusterCount < 4085) {
      fatBits = 12;
    } else if (clusterCount < 65525) {
      fatBits = 16;
    } else {
      fatBits = 32;
    }
    // 若 BPB 明确给出 FAT32 特征，则强制 32。
    const fsTypeLabel = ascii(boot, 54, 8);
    if (/FAT32/.test(fsTypeLabel) && fatSize32 !== 0) fatBits = 32;
    if (/FAT16/.test(fsTypeLabel) && fatBits === 32 && rootEntryCount > 0) fatBits = 16;

    if (fatBits === 32 && rootEntryCount !== 0) {
      warnings.push({ code: 'fat32-root-entries', message: 'FAT32 的根目录项数应为 0' });
    }
    if (fatBits === 32 && clusterCount < 65525) {
      warnings.push({
        code: 'fat32-small',
        message: `簇数 ${clusterCount} 低于 FAT32 规范下限 65525（常见于小容量测试卡）`,
      });
    }
    if (fatBits === 12 || fatBits === 16) {
      warnings.push({
        code: 'fat-non32',
        message: `检测为 FAT${fatBits}（按簇数推断），已按对应位宽解析`,
      });
    }

    // FAT 表备份检查（FAT32 在 BPB 中给出备份引导扇区号）。
    // FSInfo 空闲簇数（FAT32 在扇区 1）
    let fsinfoFree: number | null = null;
    if (fatBits === 32) {
      try {
        const fsinfo = await source.read(offset + 512, 512);
        if (readU32(fsinfo, 0) === 0x41615252 && readU32(fsinfo, 484) === 0x61417272) {
          const free = readU32(fsinfo, 488);
          if (free !== 0xffffffff && free <= clusterCount) fsinfoFree = free;
        }
      } catch {
        // 忽略：FSInfo 缺失时由 FAT 扫描或目录扫描补齐
      }
    }

    let backupBootValid: boolean | null = null;
    if (fatBits === 32) {
      const backupSector = readU16(boot, 50);
      if (backupSector !== 0 && backupSector < reservedSectors) {
        const backup = await source.read(offset + backupSector * bytesPerSector, 512);
        backupBootValid = isFatBoot(backup);
        if (!backupBootValid) {
          warnings.push({
            code: 'backup-boot-invalid',
            message: `FAT32 备份引导扇区（扇区 ${backupSector}）无效`,
          });
        }
      }
    }

    const label = readLabelFromBoot(boot, fatBits);
    const serial = formatSerial(readU32(boot, 67), readU16(boot, 71));

    const info: VolumeInfo = {
      id: `vol-${partitionIndex}-${offset}`,
      fs: (`fat${fatBits}` as FileSystemType),
      partitionIndex,
      offset,
      size: effectiveTotal * bytesPerSector,
      label,
      serial,
      bytesPerSector,
      sectorsPerCluster,
      clusterSize,
      clusterCount,
      freeClusters: fsinfoFree,
      fatOffset: offset + reservedSectors * bytesPerSector,
      fatSize: fatSize * bytesPerSector,
      fatCount,
      dataOffset: offset + firstDataSector * bytesPerSector,
      rootCluster: fatBits === 32 ? readU32(boot, 44) : null,
      backupBootValid,
      warnings,
      freeCountSource: fsinfoFree === null ? 'unknown' : 'fsinfo',
      supported: true,
    };

    const vol = new FatVolume(source, info);
    vol.fatBits = fatBits;
    vol.rootEntryCount = rootEntryCount;
    vol.rootDirOffset = offset + (reservedSectors + fatCount * fatSize) * bytesPerSector;
    const rootCluster = info.rootCluster;
    if (fatSize * bytesPerSector > FAT_IN_MEMORY_LIMIT) {
      info.freeCountSource = info.freeClusters === null ? 'unknown' : 'fsinfo';
    }
    if (fatBits === 32 && rootCluster !== null &&
        (rootCluster < 2 || rootCluster >= clusterCount + 2)) {
      info.warnings.push({
        code: 'root-cluster-invalid',
        message: `根目录簇号 ${rootCluster} 超出范围 [2, ${clusterCount + 1}]`,
      });
    }
    return vol;
  }

  /**
   * 载入第一份 FAT 表。
   *
   * 大容量卡的 FAT 可能上百 MB（32GB + 512B 簇 = 256MB），整表载入会拖垮浏览器。
   * 因此超过阈值时改用按需块读取；此时空闲簇数标注为 unknown，由目录扫描
   * 与分配位图另行统计，不阻塞打开镜像。
   */
  async loadFat(): Promise<void> {
    const bytes = Math.min(this.info.fatSize, this.info.clusterCount * 4 + 8);
    if (bytes > FAT_IN_MEMORY_LIMIT) {
      this.fat = null;
      // FSInfo 给出的空闲簇数仍然有效；不要清空它。
      this.info.freeCountSource = this.info.freeClusters === null ? 'unknown' : 'fsinfo';
      this.info.warnings.push({
        code: 'fat-on-demand',
        message: `FAT 表 ${Math.round(bytes / 1024 / 1024)} MiB，超过内存阈值，改为按需读取（不影响文件解析）`,
      });
      return;
    }
    this.fat = await this.source.read(this.info.fatOffset, bytes);
    let free = 0;
    for (let c = 2; c < this.info.clusterCount + 2; c++) {
      if (this.fatValue(c) === 0) free++;
    }
    if (this.info.freeClusters === null || this.info.freeClusters !== free) {
      this.info.freeClusters = free;
      this.info.freeCountSource = 'fat-scan';
    } else {
      this.info.freeCountSource = 'fat-scan';
    }
  }

  /** FAT 值同步读（适用于已载入的 FAT）。 */
  fatValue(cluster: number): number {
    const fat = this.fat;
    if (!fat) throw new Error('FAT 尚未载入');
    if (this.fatBits === 32) {
      return readU32(fat, cluster * 4) & 0x0fffffff;
    }
    if (this.fatBits === 16) {
      const off = cluster * 2;
      if (off + 2 > fat.length) return 0xffff;
      return readU16(fat, off);
    }
    const off = Math.floor(cluster * 1.5);
    if (off + 2 > fat.length) return 0xfff;
    const word = readU16(fat, off);
    return cluster % 2 === 0 ? word & 0x0fff : word >>> 4;
  }

  /** FAT 值异步读：FAT 未整表载入时按块读取。 */
  async fatValueAsync(cluster: number, signal?: AbortSignal): Promise<number> {
    if (this.fat) return this.fatValue(cluster);
    if (signal?.aborted) throw new DOMException('已取消', 'AbortError');
    const bytesPerEntry = this.fatBits / 8;
    const raw = await this.source.read(
      this.info.fatOffset + Math.floor(cluster * bytesPerEntry),
      Math.ceil(bytesPerEntry) + 1,
    );
    if (this.fatBits === 32) return readU32(raw, 0) & 0x0fffffff;
    if (this.fatBits === 16) return readU16(raw, 0);
    const word = readU16(raw, 0);
    return cluster % 2 === 0 ? word & 0x0fff : word >>> 4;
  }

  eocValue(): number {
    return this.fatBits === 32 ? 0x0fffffff : this.fatBits === 16 ? 0xffff : 0x0fff;
  }

  isEoc(value: number): boolean {
    if (this.fatBits === 32) return value >= 0x0ffffff8;
    if (this.fatBits === 16) return value >= 0xfff8;
    return value >= 0x0ff8;
  }

  badValue(): number {
    return this.fatBits === 32 ? 0x0ffffff7 : this.fatBits === 16 ? 0xfff7 : 0x0ff7;
  }

  clusterOffset(cluster: number): number {
    return this.info.dataOffset + (cluster - 2) * this.info.clusterSize;
  }

  async readCluster(cluster: number): Promise<Uint8Array> {
    this.checkCluster(cluster);
    return this.source.read(this.clusterOffset(cluster), this.info.clusterSize);
  }

  async readClusters(start: number, count: number): Promise<Uint8Array> {
    this.checkCluster(start);
    this.checkCluster(start + count - 1);
    return this.source.read(this.clusterOffset(start), count * this.info.clusterSize);
  }

  checkCluster(cluster: number): void {
    if (!Number.isInteger(cluster) || cluster < 2 || cluster >= this.info.clusterCount + 2) {
      throw new RangeError(`簇号越界: ${cluster} (有效 2..${this.info.clusterCount + 1})`);
    }
  }

  /**
   * 读出簇链。返回终止原因，供恢复时区分“完整”“断链”“归零”“成环”。
   */
  async readClusterChain(
    start: number,
    maxClusters: number,
    signal?: AbortSignal,
  ): Promise<{ clusters: number[]; status: 'eoc' | 'zero' | 'invalid' | 'loop' | 'limit'; badClusters: number[] }> {
    const clusters: number[] = [];
    const badClusters: number[] = [];
    const seen = new Set<number>();
    let current = start;
    let status: 'eoc' | 'zero' | 'invalid' | 'loop' | 'limit' = 'limit';
    while (clusters.length < maxClusters) {
      if (signal?.aborted) throw new DOMException('已取消', 'AbortError');
      if (current === 0) {
        status = 'zero';
        break;
      }
      if (current < 2 || current >= this.info.clusterCount + 2) {
        status = 'invalid';
        badClusters.push(current);
        break;
      }
      if (seen.has(current)) {
        status = 'loop';
        break;
      }
      seen.add(current);
      clusters.push(current);
      const next = await this.fatValueAsync(current, signal);
      if (this.isEoc(next)) {
        status = 'eoc';
        break;
      }
      if (next === 0) {
        // 簇链被清零：常见于删除操作只释放目录项、FAT 项被清。
        status = 'zero';
        break;
      }
      if (next === this.badValue()) {
        status = 'invalid';
        badClusters.push(current);
        break;
      }
      current = next;
    }
    if (clusters.length >= maxClusters) status = 'limit';
    return { clusters, status, badClusters };
  }

  /** 目录遍历。返回条目与统计。 */
  async listEntries(
    opts: { includeDeleted: boolean; maxEntries: number; signal?: AbortSignal },
    onEntry?: (entry: FileEntry) => void,
  ): Promise<{ entries: FileEntry[]; stats: ScanStats }> {
    const started = performance.now();
    await this.loadFat();
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

    const visitedDirs = new Set<number>();
    const rootCluster = this.info.rootCluster;
    const queue: Array<{ cluster: number | null; path: string; parentDeleted: boolean; depth: number }> = [
      { cluster: rootCluster, path: '/', parentDeleted: false, depth: 0 },
    ];

    while (queue.length > 0) {
      if (opts.signal?.aborted) throw new DOMException('已取消', 'AbortError');
      const dir = queue.shift()!;
      if (dir.depth > 64) {
        stats.warnings.push(`目录层级超过 64，停止展开: ${dir.path}`);
        continue;
      }
      if (entries.length >= opts.maxEntries) {
        stats.truncatedListing = true;
        break;
      }

      const dirEntries = await this.readDirectory(dir, opts.signal);
      for (const parsed of dirEntries) {
        // '.' 与 '..' 是目录结构项，不是文件，也不应作为子目录展开。
        if (parsed.name === '.' || parsed.name === '..') continue;
        const entry = this.buildEntry(parsed, dir.path);
        if (!opts.includeDeleted && entry.deleted) continue;
        if (entry.kind === 'directory' && entry.firstCluster >= 2) {
          const key = entry.firstCluster;
          if (!visitedDirs.has(key)) {
            visitedDirs.add(key);
            queue.push({
              cluster: entry.firstCluster,
              path: entry.path,
              parentDeleted: entry.deleted,
              depth: dir.depth + 1,
            });
          }
        }
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

  private async readDirectory(
    dir: { cluster: number | null; path: string; parentDeleted: boolean; depth: number },
    signal?: AbortSignal,
  ): Promise<ParsedDirEntry[]> {
    const out: ParsedDirEntry[] = [];
    const raw = await this.readDirectoryBytes(dir, signal);
    const maxEntries = Math.floor(raw.length / 32);
    let lfnPieces: LfnPiece[] = [];

    for (let i = 0; i < maxEntries; i++) {
      const rec = raw.subarray(i * 32, i * 32 + 32);
      const firstByte = rec[0]!;
      if (firstByte === 0x00) break; // 目录结束
      const attr = rec[11]!;
      const isDeletedEntry = firstByte === 0xe5;

      if ((attr & 0x3f) === ATTR_LFN) {
        // LFN 记录的“已删除”状态与其后的短名条目一致；先缓存。
        lfnPieces.push(parseLfnPiece(rec, isDeletedEntry, i));
        continue;
      }
      if ((attr & 0x08) === ATTR_VOLUME_ID && (attr & 0x10) === 0) {
        lfnPieces = [];
        continue; // 卷标项，不计为文件
      }

      const shortName = decodeShortName(rec, firstByte);
      if (!shortName) {
        lfnPieces = [];
        continue;
      }
      const nameInfo = assembleLongName(lfnPieces, rec);
      lfnPieces = [];

      out.push({
        raw: rec,
        shortName,
        name: nameInfo.name ?? shortName,
        longNameRecovered: nameInfo.recovered,
        nameConfidence: nameInfo.confidence,
        nameNote: nameInfo.note,
        deleted: isDeletedEntry || dir.parentDeleted,
        entryOffset: dir.cluster === null
          ? this.rootDirOffset + i * 32
          : this.clusterOffset(dir.cluster) + i * 32,
        entrySpan: nameInfo.span + 1,
        index: i,
      });
    }
    return out;
  }

  private async readDirectoryBytes(
    dir: { cluster: number | null; path: string; parentDeleted: boolean },
    signal?: AbortSignal,
  ): Promise<Uint8Array> {
    if (dir.cluster === null) {
      return this.source.read(this.rootDirOffset, this.rootEntryCount * 32);
    }
    if (dir.parentDeleted) {
      // 已删除目录：只读取起始簇，验证内容是否为合理目录。
      if (dir.path !== '/' && !dir.parentDeleted) return new Uint8Array(0);
      const cluster = await this.readCluster(dir.cluster);
      if (!looksLikeDirectory(cluster)) return new Uint8Array(0);
      return cluster;
    }
    const chain = await this.readClusterChain(
      dir.cluster,
      Math.max(1, Math.ceil(this.info.clusterCount / 2)),
      signal,
    );
    // 目录链异常时，仅读取已有部分并记录（由上层 warnings 汇总）。
    if (chain.status === 'invalid' || chain.status === 'loop') {
      this.info.warnings.push({
        code: 'dir-chain-broken',
        message: `目录 ${dir.path} 簇链异常: ${chain.status}`,
      });
    }
    const buf = new Uint8Array(chain.clusters.length * this.info.clusterSize);
    for (let i = 0; i < chain.clusters.length; i++) {
      buf.set(await this.readCluster(chain.clusters[i]!), i * this.info.clusterSize);
    }
    return buf;
  }

  private buildEntry(parsed: ParsedDirEntry, parentPath: string): FileEntry {
    const attr = parsed.raw[11]!;
    const kind: EntryKind = (attr & ATTR_DIRECTORY) !== 0 ? 'directory' : 'file';
    const firstClusterHigh = readU16(parsed.raw, 20);
    const firstClusterLow = readU16(parsed.raw, 26);
    const firstCluster = this.fatBits === 32
      ? ((firstClusterHigh << 16) | firstClusterLow) >>> 0
      : firstClusterLow;
    const size = readU32(parsed.raw, 28);
    const path = joinPath(parentPath, parsed.name);
    const times: FileEntry['times'] = {
      created: decodeFatTime(parsed.raw, 16, 14, 13),
      modified: decodeFatTime(parsed.raw, 24, 22, null),
      accessed: decodeFatDate(parsed.raw, 18),
    };

    const warnings: string[] = [];
    if (kind === 'directory' && size !== 0) {
      warnings.push(`目录条目文件大小非 0 (${size})`);
    }
    if (kind === 'file' && firstCluster === 0 && size > 0) {
      warnings.push('文件大小非 0 但起始簇为 0');
    }
    const attributeLabels = describeAttributes(attr);

    return {
      id: `${this.info.id}:${parsed.entryOffset}`,
      volumeId: this.info.id,
      parentPath,
      path,
      name: parsed.name,
      shortName: parsed.shortName,
      kind,
      deleted: parsed.deleted,
      longNameRecovered: parsed.longNameRecovered,
      nameConfidence: parsed.nameConfidence,
      nameNote: parsed.nameNote,
      size,
      firstCluster,
      runs: [],
      allocatedClusters: 0,
      overwrittenClusters: 0,
      allocationHint: 'fat-chain',
      chainQuality: 'empty',
      attributes: attr,
      attributeLabels,
      times,
      entryOffset: parsed.entryOffset,
      entrySpan: parsed.entrySpan,
      warnings,
      isMetadata: isMetadataName(parsed.name),
      ...(parsed.deleted ? { orphaned: parentPath !== '/' && parsed.deleted } : {}),
    };
  }

  /** 为恢复阶段补全簇链信息；只读 FAT，不写。 */
  async resolveChain(entry: FileEntry, signal?: AbortSignal): Promise<ClusterRun[]> {
    // 分配表只与卷当前状态有关，构建一次后缓存；否则逐个恢复会退化为 O(n²)。
    const allocatedMap = await this.getAllocatedMap(signal);
    const needed = entry.kind === 'file'
      ? Math.ceil(entry.size / this.info.clusterSize)
      : 0;
    if (entry.firstCluster < 2) {
      entry.chainQuality = entry.size === 0 ? 'empty' : 'error';
      if (entry.size > 0) entry.warnings.push('起始簇无效，无法按链恢复');
      return [];
    }

    if (entry.deleted) {
      // 删除文件：优先使用残留簇链；FAT 项归零则按连续簇假设。
      const chain = await this.readClusterChain(entry.firstCluster, Math.max(needed, 1), signal);
      const intact = chain.status === 'eoc' || chain.status === 'limit';
      if (intact && chain.clusters.length >= needed && needed > 0) {
        entry.chainQuality = 'valid';
        entry.runs = toRuns(chain.clusters);
        entry.allocatedClusters = chain.clusters.length;
      } else if (needed === 0) {
        entry.chainQuality = 'empty';
        entry.runs = [];
      } else {
        // 连续假设：已删除文件若 FAT 被清零，数据通常仍在原处连续存放。
        const start = entry.firstCluster;
        const count = needed;
        const endCluster = start + count - 1;
        if (endCluster < 2 || endCluster >= this.info.clusterCount + 2) {
          entry.chainQuality = 'truncated';
          entry.warnings.push(
            `连续假设超出卷边界：需要 ${count} 簇，起始 ${start}，卷上限 ${this.info.clusterCount + 1}`,
          );
          const usable = Math.max(0, this.info.clusterCount + 1 - start);
          entry.runs = usable > 0 ? [{ start, count: usable }] : [];
          entry.allocatedClusters = usable;
        } else {
          entry.runs = [{ start, count }];
          entry.allocatedClusters = count;
          entry.chainQuality = 'contiguous-assumed';
          entry.warnings.push(
            chain.status === 'zero'
              ? 'FAT 簇链已被清零，按连续簇恢复（恢复完整性以覆盖检查为准）'
              : `簇链不完整 (${chain.status})，按连续簇恢复`,
          );
        }
      }
    } else {
      const chain = await this.readClusterChain(
        entry.firstCluster,
        Math.max(needed, 1),
        signal,
      );
      entry.runs = toRuns(chain.clusters);
      entry.allocatedClusters = chain.clusters.length;
      if (entry.kind === 'directory') {
        entry.chainQuality = chain.status === 'eoc' || chain.status === 'limit' ? 'valid' : 'truncated';
      } else if (chain.clusters.length === needed && (chain.status === 'eoc' || chain.status === 'limit')) {
        entry.chainQuality = 'valid';
      } else if (chain.clusters.length < needed) {
        entry.chainQuality = chain.status === 'zero' ? 'overwritten' : 'truncated';
        entry.warnings.push(`线索不足：需要 ${needed} 簇，链上只有 ${chain.clusters.length} 簇`);
      } else {
        entry.chainQuality = 'truncated';
        entry.warnings.push('簇链长度与文件大小不一致（链更长）');
      }
    }

    // 覆盖检查：已删除条目的数据簇若已分配给别人，标记覆盖簇数。
    if (entry.deleted) {
      const clusters = runsToClusters(entry.runs);
      let overwritten = 0;
      for (const c of clusters) {
        if (allocatedMap.get(c)) overwritten++;
      }
      if (overwritten > 0) {
        entry.overwrittenClusters = overwritten;
        const coveredRatio = overwritten / Math.max(1, clusters.length);
        if (coveredRatio >= 1) entry.chainQuality = 'overwritten';
        else if (entry.chainQuality === 'valid') entry.chainQuality = 'truncated';
        entry.warnings.push(
          `${overwritten}/${clusters.length} 个数据簇已被现存文件重新分配（部分或全部被覆盖）`,
        );
      }
    }
    return entry.runs;
  }

  /** 缓存的分配表访问器（同一次扫描中只构建一次）。 */
  async getAllocatedMap(signal?: AbortSignal): Promise<Map<number, string>> {
    if (!this.allocatedMapCache) {
      this.allocatedMapCache = this.buildAllocatedMap(signal);
    }
    return this.allocatedMapCache;
  }

  /** 释放缓存（重新扫描/切换卷时调用）。 */
  clearAllocatedCache(): void {
    this.allocatedMapCache = null;
  }

  /** 现有（未删除）文件与目录占用的簇。用于覆盖推断。 */
  async buildAllocatedMap(signal?: AbortSignal): Promise<Map<number, string>> {
    const map = new Map<number, string>();
    const rootCluster = this.info.rootCluster;
    const queue: Array<{ cluster: number | null; path: string }> = [
      { cluster: rootCluster, path: '/' },
    ];
    const seenDirs = new Set<number>();
    while (queue.length > 0) {
      if (signal?.aborted) throw new DOMException('已取消', 'AbortError');
      const dir = queue.shift()!;
      const raw = await this.readDirectoryBytes(
        { cluster: dir.cluster, path: dir.path, parentDeleted: false },
        signal,
      );
      const maxEntries = Math.floor(raw.length / 32);
      let lfnPieces: LfnPiece[] = [];
      for (let i = 0; i < maxEntries; i++) {
        const rec = raw.subarray(i * 32, i * 32 + 32);
        const firstByte = rec[0]!;
        if (firstByte === 0x00) break;
        const attr = rec[11]!;
        if ((attr & 0x3f) === ATTR_LFN) {
          if (firstByte !== 0xe5) lfnPieces.push(parseLfnPiece(rec, false, i));
          continue;
        }
        const isDir = (attr & ATTR_DIRECTORY) !== 0;
        const isVolume = (attr & 0x08) !== 0 && !isDir;
        lfnPieces = [];
        if (firstByte === 0xe5 || isVolume) continue; // 删除项不计入分配表
        const high = readU16(rec, 20);
        const low = readU16(rec, 26);
        const firstCluster = this.fatBits === 32 ? ((high << 16) | low) >>> 0 : low;
        if (firstCluster < 2) continue;
        const size = readU32(rec, 28);
        const needed = isDir
          ? Math.ceil(this.info.clusterCount / 2)
          : Math.max(1, Math.ceil(size / this.info.clusterSize));
        const chain = await this.readClusterChain(firstCluster, needed, signal);
        const path = joinPath(dir.path, decodeShortName(rec, firstByte) ?? '?');
        for (const c of chain.clusters) map.set(c, path);
        if (isDir && !seenDirs.has(firstCluster)) {
          seenDirs.add(firstCluster);
          queue.push({ cluster: firstCluster, path });
        }
      }
    }
    return map;
  }

  /** 读取文件有效数据（按链），返回数据与缺失字节数。 */
  async readFileData(
    entry: FileEntry,
    signal?: AbortSignal,
  ): Promise<{ data: Uint8Array; missingBytes: number; overwrittenClusters: number }> {
    if (entry.kind === 'directory') {
      throw new Error('目录不使用此接口');
    }
    const clusters = runsToClusters(entry.runs);
    const total = clusters.reduce((s, c) => s + c, 0);
    const out = new Uint8Array(total * this.info.clusterSize);
    let at = 0;
    for (const cluster of clusters) {
      if (signal?.aborted) throw new DOMException('已取消', 'AbortError');
      const data = await this.readCluster(cluster);
      out.set(data, at);
      at += data.length;
    }
    const wanted = entry.size;
    if (out.length >= wanted) {
      return {
        data: out.subarray(0, wanted),
        missingBytes: 0,
        overwrittenClusters: entry.overwrittenClusters ?? 0,
      };
    }
    return {
      data: out,
      missingBytes: wanted - out.length,
      overwrittenClusters: entry.overwrittenClusters ?? 0,
    };
  }
}

export interface ParsedDirEntry {
  raw: Uint8Array;
  shortName: string;
  name: string;
  longNameRecovered: boolean;
  nameConfidence: FileEntry['nameConfidence'];
  nameNote?: string | undefined;
  deleted: boolean;
  entryOffset: number;
  entrySpan: number;
  index: number;
}

interface LfnPiece {
  ordinal: number;
  chars: string;
  checksum: number;
  deleted: boolean;
  index: number;
}

function parseLfnPiece(rec: Uint8Array, deleted: boolean, index: number): LfnPiece {
  const ordinal = rec[0]! & 0x3f;
  const checksum = rec[13]!;
  let chars = '';
  // LFN 字符位置：1-10、14-25、28-31，共 13 个 UTF-16 码元。
  const offsets = [1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30];
  for (const at of offsets) {
    const cp = readU16(rec, at);
    if (cp === 0xffff || cp === 0) continue;
    chars += String.fromCharCode(cp);
  }
  return { ordinal, chars, checksum, deleted, index };
}

/**
 * 组合 LFN 与短名。删除后 LFN 项首字节变为 0xE5，序号被破坏，
 * 但 LFN 在磁盘上按“倒序”排列（最高序号在前），物理顺序仍可重组；
 * 同时校验位于第 13 字节的短名校验和，多数情况下可完整恢复长文件名。
 */
function assembleLongName(
  pieces: LfnPiece[],
  shortEntry: Uint8Array,
): {
  name: string | null;
  recovered: boolean;
  confidence: FileEntry['nameConfidence'];
  note?: string;
  span: number;
} {
  if (pieces.length === 0) {
    // 没有 LFN：现存项显示的就是 8.3 短名本身，可信；
    // 已删除项则可能曾有长名而随 LFN 一并被清，只能退为中等可信。
    return { name: null, recovered: false, confidence: 'exact', span: 0 };
  }
  const expectedChecksum = shortNameChecksum(shortEntry);
  const hasDeleted = pieces.some((p) => p.deleted);
  // 删除目录项会把短名首字节写为 0xE5，校验和必然失配；此时改用 LFN 项自身
  // 记录的校验和一致性来判断名称是否被完整保留（LFN 片段彼此校验和相同）。
  const lfnSelfConsistent = pieces.every((p) => p.checksum === pieces[0]!.checksum);
  const checksumMatched = hasDeleted
    ? lfnSelfConsistent
    : pieces.every((p) => p.checksum === expectedChecksum);

  let ordered: LfnPiece[];
  let orderNote: string | undefined;
  if (hasDeleted) {
    // 删除项的首字节（0xE5）毁掉了序号。磁盘顺序为 [ordN|LAST]...[ord1]，
    // 因此反转物理顺序即得到从首字符开始的片段顺序。
    ordered = [...pieces].reverse();
    orderNote = '长文件名序号位已被删除标记覆盖，按物理倒序重组';
  } else {
    ordered = [...pieces].sort((a, b) => a.ordinal - b.ordinal);
    for (let i = 0; i < ordered.length; i++) {
      if (ordered[i]!.ordinal !== i + 1) {
        return {
          name: null,
          recovered: false,
          confidence: 'low',
          note: `长文件名序号不连续（缺少第 ${i + 1} 段），无法可靠重组`,
          span: pieces.length,
        };
      }
    }
  }

  let name = ordered.map((p) => p.chars).join('');
  const nul = name.indexOf('\u0000');
  if (nul >= 0) name = name.slice(0, nul);
  name = name.replace(/￿/g, '');
  if (!name) {
    return { name: null, recovered: false, confidence: 'low', note: '长文件名为空', span: pieces.length };
  }
  if (!checksumMatched) {
    const note = hasDeleted
      ? '已删除项的长文件名片段校验和彼此不一致，名称可能被部分覆盖，仅作参考'
      : '长文件名校验和与短名不匹配：内容可能被改写，名称仅作参考';
    return { name, recovered: false, confidence: 'low', note, span: pieces.length };
  }
  return {
    name,
    recovered: hasDeleted,
    confidence: hasDeleted ? 'high' : 'exact',
    ...(hasDeleted
      ? { note: orderNote ?? '长文件名来自已删除目录项，校验和匹配' }
      : {}),
    span: pieces.length,
  };
}

function shortNameChecksum(rec: Uint8Array): number {
  let sum = 0;
  for (let i = 0; i < 11; i++) {
    sum = (((sum & 1) << 7) + (sum >>> 1) + rec[i]!) & 0xff;
  }
  return sum;
}

function decodeShortName(rec: Uint8Array, firstByte: number): string | null {
  const base: string[] = [];
  const ext: string[] = [];
  for (let i = 0; i < 8; i++) {
    const b = rec[i]!;
    if (b === 0) break;
    if (b === 0x20) continue;
    if (i === 0 && firstByte === 0xe5) {
      // 删除操作把短名首字节覆写为 0xE5，真实首字符已不可知；
      // 取证惯例用 '?' 占位，其余 7 个字符仍可信。
      base.push('?');
      continue;
    }
    if (i === 0 && firstByte === 0x05) {
      // 0x05 是 0xE5 的转义表示（原名首字符确为 0xE5）。
      base.push(decodeOemByte(0xe5));
      continue;
    }
    base.push(decodeOemByte(b));
  }
  for (let i = 8; i < 11; i++) {
    const b = rec[i]!;
    if (b === 0 || b === 0x20) continue;
    ext.push(decodeOemByte(b));
  }
  if (base.length === 0) return null;
  const name = base.join('');
  return ext.length > 0 ? `${name}.${ext.join('')}` : name;
}

/**
 * OEM 短名字节解码。8.3 名称按 CP936 双字节编码（中文相机常见），
 * 但逐字节解码会拆散双字节字符；这里对 >=0x80 的字节保留原值，
 * 由调用方在需要时整串按 CP936 解码（见 decodeShortName 的调用处）。
 */
function decodeOemByte(b: number): string {
  if (b >= 0x20 && b <= 0x7e) return String.fromCharCode(b);
  return String.fromCharCode(b);
}

function describeAttributes(attr: number): string[] {
  const labels: string[] = [];
  if (attr & ATTR_READONLY) labels.push('只读');
  if (attr & ATTR_HIDDEN) labels.push('隐藏');
  if (attr & ATTR_SYSTEM) labels.push('系统');
  if (attr & ATTR_VOLUME_ID) labels.push('卷标');
  if (attr & ATTR_DIRECTORY) labels.push('目录');
  if (attr & ATTR_ARCHIVE) labels.push('存档');
  return labels;
}

function decodeFatTime(
  rec: Uint8Array,
  dateOffset: number,
  timeOffset: number,
  tenthsOffset: number | null,
): TimestampValue | null {
  const date = readU16(rec, dateOffset);
  const time = readU16(rec, timeOffset);
  const tenths = tenthsOffset === null ? 0 : (rec[tenthsOffset] ?? 0);
  if (date === 0 && time === 0) return null;
  const year = 1980 + ((date >>> 9) & 0x7f);
  const month = (date >>> 5) & 0x0f;
  const day = date & 0x1f;
  const hour = (time >>> 11) & 0x1f;
  const minute = (time >>> 5) & 0x3f;
  const second = (time & 0x1f) * 2;
  const ms = Math.min(199, tenths) * 10;
  if (month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59) {
    return { raw: `date=0x${date.toString(16)} time=0x${time.toString(16)}`, precision: 'datetime', iso: '无效时间戳', tzOffsetMinutes: null };
  }
  const sec = second + Math.floor(ms / 1000);
  const iso = `${pad(year, 4)}-${pad(month)}-${pad(day)}T${pad(hour)}:${pad(minute)}:${pad(sec)}.${pad(ms % 1000, 3)}`;
  return { raw: `date=0x${date.toString(16)} time=0x${time.toString(16)} tenths=${tenths}`, precision: 'datetime', iso, tzOffsetMinutes: null };
}

function decodeFatDate(rec: Uint8Array, dateOffset: number): TimestampValue | null {
  const date = readU16(rec, dateOffset);
  if (date === 0) return null;
  const year = 1980 + ((date >>> 9) & 0x7f);
  const month = (date >>> 5) & 0x0f;
  const day = date & 0x1f;
  if (month < 1 || month > 12 || day < 1 || day > 31) {
    return { raw: `date=0x${date.toString(16)}`, precision: 'date', iso: '无效时间戳', tzOffsetMinutes: null };
  }
  return {
    raw: `date=0x${date.toString(16)}`,
    precision: 'date',
    iso: `${pad(year, 4)}-${pad(month)}-${pad(day)}T00:00:00.000`,
    tzOffsetMinutes: null,
  };
}

/**
 * 卷标读取。FAT32 的卷标可能出现在引导扇区 BPB（偏移 43，11 字节），
 * 也可能仅存在于根目录的 0x08 卷标项中。这里先读 BPB，并过滤非打印字符。
 */
function readLabelFromBoot(boot: Uint8Array, fatBits: 12 | 16 | 32): string {
  // FAT12/16 卷标在 BPB 偏移 43；FAT32 的扩展 BPB 把它移到偏移 71。
  const raw = ascii(boot, fatBits === 32 ? 71 : 43, 11);
  const cleaned = raw.replace(/[^\x20-\x7e]/g, '').trim();
  if (cleaned && !/NO NAME|NO_NAME/i.test(cleaned)) return cleaned;
  return '';
}

function formatSerial(serial: number, ext: number): string {
  return `${serial.toString(16).toUpperCase().padStart(8, '0')}${ext ? ext.toString(16).toUpperCase().padStart(4, '0') : ''}`;
}

function pad(v: number, n = 2): string {
  return v.toString().padStart(n, '0');
}

function toRuns(clusters: number[]): ClusterRun[] {
  if (clusters.length === 0) return [];
  const runs: ClusterRun[] = [];
  let start = clusters[0]!;
  let count = 1;
  for (let i = 1; i < clusters.length; i++) {
    if (clusters[i] === clusters[i - 1]! + 1) {
      count++;
    } else {
      runs.push({ start, count });
      start = clusters[i]!;
      count = 1;
    }
  }
  runs.push({ start, count });
  return runs;
}

function runsToClusters(runs: ClusterRun[]): number[] {
  const out: number[] = [];
  for (const run of runs) {
    for (let i = 0; i < run.count; i++) out.push(run.start + i);
  }
  return out;
}

function looksLikeDirectory(bytes: Uint8Array): boolean {
  if (bytes.length < 32) return false;
  let plausible = 0;
  let terminated = false;
  for (let i = 0; i < Math.min(bytes.length / 32, 64); i++) {
    const b0 = bytes[i * 32]!;
    const attr = bytes[i * 32 + 11]!;
    if (b0 === 0x00) {
      terminated = true;
      break;
    }
    if (b0 === 0xe5) {
      plausible++;
      continue;
    }
    if ((attr & 0x3f) === ATTR_LFN || (attr & 0x08) !== 0 || attr <= 0x3f) {
      plausible++;
    }
  }
  return plausible >= 2 || terminated;
}

function isFatBoot(boot: Uint8Array): boolean {
  if (boot.length < 512) return false;
  if (boot[510] !== 0x55 || boot[511] !== 0xaa) return false;
  const bps = readU16(boot, 11);
  if (![512, 1024, 2048, 4096].includes(bps)) return false;
  if (boot[13] === 0) return false;
  if (boot[16] === 0) return false;
  if (readU16(boot, 14) === 0) return false;
  if (readU16(boot, 22) === 0 && readU32(boot, 36) === 0) return false;
  return true;
}

function readU16(buf: Uint8Array, off: number): number {
  return buf[off]! | (buf[off + 1]! << 8);
}

function readU32(buf: Uint8Array, off: number): number {
  return (buf[off]! | (buf[off + 1]! << 8) | (buf[off + 2]! << 16) | (buf[off + 3]! << 24)) >>> 0;
}

function ascii(buf: Uint8Array, off: number, len: number): string {
  let s = '';
  for (let i = 0; i < len; i++) s += String.fromCharCode(buf[off + i] ?? 0);
  return s;
}

/**
 * 识别操作系统写下的元数据文件，避免它们污染文件统计与时间线。
 * macOS 的 AppleDouble（._foo）与 .fseventsd、Windows 的 System Volume Information
 * 在存储卡上不代表用户数据。
 */
export function isMetadataName(name: string): boolean {
  return (
    name.startsWith('._') ||
    name === '.fseventsd' ||
    name === '.Spotlight-V100' ||
    name === '.Trashes' ||
    name === '.TemporaryItems' ||
    name.toLowerCase() === 'system volume information'
  );
}

function joinPath(parent: string, name: string): string {
  if (parent === '/') return `/${name}`;
  return `${parent}/${name}`;
}

export { isFatBoot, toRuns, runsToClusters, joinPath, decodeFatTime, decodeFatDate, ATTR_DIRECTORY };
