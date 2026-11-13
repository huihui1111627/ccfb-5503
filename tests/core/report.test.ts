/**
 * PDF 报告回归测试：确保中文可提取、关键字段齐全、字体嵌入成功。
 * 需要真实字体文件 public/fonts/NotoSansSC-Regular.ttf。
 */
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { generateReport, type ReportInput } from '../../src/core/report/pdf';
import type { AnalysisRecord, CaseRecord, CustodyEvent, ImageRecord } from '../../src/core/types';

// pdf.ts 使用 fetch 加载字体；Node 测试环境注入一个基于 fs 的 fetch。
const g = globalThis as unknown as { fetch: typeof fetch };
if (!g.fetch || !String(g.fetch).includes('node')) {
  const realFetch = g.fetch;
  g.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (url.endsWith('/fonts/NotoSansSC-Regular.ttf')) {
      const data = readFileSync('public/fonts/NotoSansSC-Regular.ttf');
      return new Response(new Uint8Array(data), { status: 200 });
    }
    if (!realFetch) throw new Error(`测试环境无法加载 ${url}`);
    return realFetch(input, init);
  }) as typeof fetch;
}

const now = '2026-03-05T03:00:00.000Z';
const caseRecord: CaseRecord = {
  id: 'case-1',
  caseNumber: '事故鉴 2026-031',
  title: '某路段行车记录仪存储卡取证',
  examiner: '李四',
  organization: '某事故鉴定所',
  notes: '送检 32GB microSD 一张，卡面标注 DASHCAM。',
  timezoneOffsetMinutes: 480,
  createdAt: now,
  updatedAt: now,
};

const image: ImageRecord = {
  id: 'img-1',
  caseId: 'case-1',
  fileName: 'card.dd',
  size: 33_554_432_000,
  md5: 'd41d8cd98f00b204e9800998ecf8427e',
  sha256: 'a'.repeat(64),
  hashStatus: 'complete',
  importedAt: now,
  lastVerifiedAt: now,
  verification: 'verified',
  partitions: [
    {
      index: 0,
      scheme: 'mbr',
      offset: 1_048_576,
      size: 33_552_000_000,
      typeCode: '0x0c',
      typeLabel: 'FAT32 (LBA)',
      bootable: false,
      volume: {
        id: 'v0', fs: 'fat32', partitionIndex: 0, offset: 1_048_576, size: 33_552_000_000,
        label: 'DASHCAM', serial: '1A2B3C4D', bytesPerSector: 512, sectorsPerCluster: 64,
        clusterSize: 32768, clusterCount: 1_024_000, freeClusters: 900_000, fatOffset: 1_064_960,
        fatSize: 4_096_000, fatCount: 2, dataOffset: 9_256_960, rootCluster: 2, backupBootValid: true,
        warnings: [], freeCountSource: 'fsinfo', supported: true,
      },
    },
  ],
};

const analysis: AnalysisRecord = {
  id: 'an-1',
  caseId: 'case-1',
  imageId: 'img-1',
  createdAt: now,
  volumes: image.partitions.map((p) => p.volume!),
  entries: [],
  stats: {
    files: 120, deletedFiles: 37, directories: 6, deletedDirectories: 2,
    totalBytes: 20_000_000_000, recoveredName: 31, warnings: [], durationMs: 4200, truncatedListing: false,
  },
  carveHits: [],
  integrity: {
    hashBefore: { md5: 'd41d8cd98f00b204e9800998ecf8427e', sha256: 'a'.repeat(64) },
    hashAfter: { md5: 'd41d8cd98f00b204e9800998ecf8427e', sha256: 'a'.repeat(64) },
    identical: true, verifiedAt: now,
  },
};

const custodyEvents: CustodyEvent[] = [
  { seq: 1, caseId: 'case-1', at: now, actor: '李四', action: 'case.create', actionLabel: '创建案件', target: '事故鉴 2026-031', details: {}, prevHash: 'GENESIS', hash: 'f'.repeat(64) },
];

function input(): ReportInput {
  return {
    caseRecord, image, analysis, entries: [], recoveries: [], carveHits: [],
    custodyEvents, custodyValid: true, generatedAt: now,
  };
}

describe('PDF 报告', () => {
  it('生成可解析的 PDF，中文与关键字段齐全', async () => {
    const bytes = await generateReport(input());
    expect(bytes.length).toBeGreaterThan(10_000);
    expect(String.fromCharCode(...bytes.subarray(0, 5))).toBe('%PDF-');
    const tmp = '/tmp/report-test.pdf';
    const { writeFileSync } = await import('node:fs');
    writeFileSync(tmp, bytes);
    const text = execFileSync('pdftotext', [tmp, '-'], { encoding: 'utf8' });
    expect(text).toContain('存储卡取证分析报告');
    expect(text).toContain('事故鉴 2026-031');
    expect(text).toContain('SHA-256');
    expect(text).toContain('分析前后哈希一致');
    expect(text).toContain('监管链');
    expect(text).toContain('方法与局限');
    expect(text).not.toContain('□□□');
  });

  it('生僻字不导致生成失败，替换为占位符', async () => {
    const withRare = input();
    withRare.caseRecord = { ...withRare.caseRecord, title: '生僻字𪚥与扩展区𠀀测试', examiner: '张𪚥' };
    const bytes = await generateReport(withRare);
    expect(bytes.length).toBeGreaterThan(10_000);
    const { writeFileSync } = await import('node:fs');
    writeFileSync('/tmp/report-rare.pdf', bytes);
    const text = execFileSync('pdftotext', ['/tmp/report-rare.pdf', '-'], { encoding: 'utf8' });
    expect(text).toContain('□');
    expect(text).toContain('生僻字');
  });

  it('字体被嵌入且可做文本提取', async () => {
    const bytes = await generateReport(input());
    const { writeFileSync } = await import('node:fs');
    writeFileSync('/tmp/report-fonts.pdf', bytes);
    const fonts = execFileSync('pdffonts', ['/tmp/report-fonts.pdf'], { encoding: 'utf8' });
    expect(fonts).toMatch(/NotoSansSC/);
    expect(fonts).toMatch(/yes/);
  });
});
