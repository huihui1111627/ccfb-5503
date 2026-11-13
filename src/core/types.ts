/**
 * 领域类型。所有取证结果都围绕“可复核”设计：每个对象带定位偏移、
 * 恢复方法、完整性状态与哈希，便于在报告与监管链中相互印证。
 */

export type FileSystemType = 'fat32' | 'exfat' | 'fat16' | 'fat12' | 'unknown';

export type PartitionScheme = 'mbr' | 'gpt' | 'superfloppy';

export interface PartitionInfo {
  index: number;
  scheme: PartitionScheme;
  offset: number;
  size: number;
  typeCode: string;
  typeLabel: string;
  bootable: boolean;
  name?: string;
  guid?: string;
  volume?: VolumeInfo;
}

export interface VolumeWarnings {
  code: string;
  message: string;
}

export interface VolumeInfo {
  id: string;
  fs: FileSystemType;
  partitionIndex: number;
  offset: number;
  size: number;
  label: string;
  serial: string;
  bytesPerSector: number;
  sectorsPerCluster: number;
  clusterSize: number;
  clusterCount: number;
  freeClusters: number | null;
  fatOffset: number;
  fatSize: number;
  fatCount: number;
  dataOffset: number;
  rootCluster: number | null;
  backupBootValid?: boolean | null;
  warnings: VolumeWarnings[];
  /** 卷管理器不做全量校验；此字段说明空闲簇数来源。 */
  freeCountSource: 'fsinfo' | 'bitmap' | 'fat-scan' | 'unknown';
  supported: boolean;
}

export interface TimestampValue {
  /** 磁盘中的原始打包值，便于复核。 */
  raw: string;
  /** 精度：FAT/exFAT 的访问时间仅记录日期，时刻不可知。 */
  precision: 'datetime' | 'date';
  /**
   * ISO 表示。exFAT 带显式 UTC 偏移；FAT 为卷内本地时间，不带时区。
   */
  iso: string;
  /** UTC 偏移（分钟），FAT 为 null，需按案件时区归一化。 */
  tzOffsetMinutes: number | null;
  /** 10ms 增量（若有）。 */
  tenths?: number;
}

export interface FileTimes {
  created: TimestampValue | null;
  modified: TimestampValue | null;
  accessed: TimestampValue | null;
}

export type ChainQuality =
  | 'valid'
  | 'contiguous-assumed'
  | 'truncated'
  | 'empty'
  | 'loop'
  | 'overwritten'
  | 'error';

export interface ClusterRun {
  start: number;
  count: number;
}

export type EntryKind = 'file' | 'directory';

export interface FileEntry {
  id: string;
  volumeId: string;
  parentPath: string;
  path: string;
  name: string;
  shortName: string | null;
  kind: EntryKind;
  deleted: boolean;
  longNameRecovered: boolean;
  /** 名称可信度：exact / high / medium / low。 */
  nameConfidence: 'exact' | 'high' | 'medium' | 'low';
  nameNote?: string | undefined;
  /** 系统/元数据文件（如 macOS ._ 资源分支、.fseventsd），默认不计入案情统计。 */
  isMetadata?: boolean;
  size: number;
  firstCluster: number;
  runs: ClusterRun[];
  allocatedClusters: number;
  /** 已删除文件的数据簇中被现存条目重新分配的数量。 */
  overwrittenClusters: number;
  /** 分配方式提示：exFAT 的 NoFatChain / FAT 链 / 未知。 */
  allocationHint: 'contiguous' | 'fat-chain' | 'unknown';
  chainQuality: ChainQuality;
  attributes: number;
  attributeLabels: string[];
  times: FileTimes;
  /** 目录项在镜像中的绝对偏移。 */
  entryOffset: number;
  /** 该条目使用的目录项总数（短项 + LFN）。 */
  entrySpan: number;
  volumeSerial?: string;
  /** 位于已删除目录中的条目标记。 */
  orphaned?: boolean;
  warnings: string[];
}

export type RecoveryStatus = 'complete' | 'partial' | 'overwritten' | 'failed';

export interface RecoveryResult {
  entryId: string;
  volumeId: string;
  name: string;
  path: string;
  deleted: boolean;
  method: 'fat-chain' | 'contiguous' | 'exfat-chain' | 'exfat-contiguous' | 'none';
  status: RecoveryStatus;
  requestedSize: number;
  recoveredSize: number;
  missingBytes: number;
  overwrittenClusters: number;
  hash: {
    md5: string;
    sha256: string;
  } | null;
  reasons: string[];
  runs: ClusterRun[];
  /** 恢复数据（浏览器 Blob），大文件不会强制载入连续内存。 */
  blob?: Blob;
  extractedAt: string;
}

export type CarveType = 'mp4' | 'jpeg';

export interface CarveHit {
  id: string;
  volumeId: string;
  type: CarveType;
  offset: number;
  size: number;
  complete: boolean;
  reasons: string[];
  hash: {
    md5: string;
    sha256: string;
  };
  details: Record<string, string | number>;
  blob?: Blob;
  carvedAt: string;
  /** 是否与已恢复文件/其他雕刻对象重叠。 */
  overlap?: string;
}

export interface UnallocatedExtent {
  start: number;
  size: number;
}

export interface ScanStats {
  files: number;
  deletedFiles: number;
  directories: number;
  deletedDirectories: number;
  totalBytes: number;
  recoveredName: number;
  /** 被识别为系统元数据并排除统计的条目数。 */
  metadataFiles?: number;
  warnings: string[];
  durationMs: number;
  truncatedListing: boolean;
}

export type CustodyAction =
  | 'case.create'
  | 'case.update'
  | 'image.import'
  | 'image.hash'
  | 'image.verify'
  | 'volume.detect'
  | 'file.scan'
  | 'file.recover'
  | 'carve.scan'
  | 'timeline.build'
  | 'report.generate'
  | 'custody.export'
  | 'custody.verify';

export interface CustodyEvent {
  seq: number;
  caseId: string;
  at: string;
  actor: string;
  action: CustodyAction;
  actionLabel: string;
  target: string;
  details: Record<string, unknown>;
  prevHash: string;
  hash: string;
}

export interface CaseRecord {
  id: string;
  caseNumber: string;
  title: string;
  examiner: string;
  organization: string;
  notes: string;
  /** FAT 时间戳归一化所用时区，分钟（东八区 = 480）。 */
  timezoneOffsetMinutes: number;
  createdAt: string;
  updatedAt: string;
}

export interface ImageRecord {
  id: string;
  caseId: string;
  fileName: string;
  size: number;
  md5: string | null;
  sha256: string | null;
  hashStatus: 'pending' | 'hashing' | 'complete' | 'failed';
  importedAt: string;
  lastVerifiedAt: string | null;
  verification: 'unverified' | 'verified' | 'mismatch' | 'pending';
  partitions: PartitionInfo[];
}

export interface AnalysisRecord {
  id: string;
  caseId: string;
  imageId: string;
  createdAt: string;
  volumes: VolumeInfo[];
  entries: FileEntry[];
  stats: ScanStats;
  carveHits: CarveHit[];
  /** 分析期间的完整性复验结果。 */
  integrity: {
    hashBefore: { md5: string; sha256: string } | null;
    hashAfter: { md5: string; sha256: string } | null;
    identical: boolean | null;
    verifiedAt: string | null;
  };
}

export interface TimelineEvent {
  id: string;
  entryId: string;
  volumeId: string;
  kind: 'created' | 'modified' | 'accessed';
  at: string;
  atEpoch: number;
  timezoneKnown: boolean;
  /** 仅日期精度（访问时间），排序时仍用占位时刻但界面明示。 */
  dateOnly: boolean;
  name: string;
  path: string;
  deleted: boolean;
  fileKind: EntryKind;
  size: number;
  warnings: string[];
}
