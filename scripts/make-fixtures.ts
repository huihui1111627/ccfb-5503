/**
 * 生成端到端测试镜像（含已知答案），同时写一份答案 JSON。
 * 用法：npx tsx scripts/make-fixtures.ts [--large]
 */
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { buildFat32 } from '../tests/fixtures/fat32-builder';
import { buildExfat } from '../tests/fixtures/exfat-builder';
import { makeMp4, tinyJpeg } from '../tests/fixtures/media';

const outDir = join(process.cwd(), 'fixtures');
mkdirSync(outDir, { recursive: true });

const at = (s: string): Date => new Date(s);
const large = process.argv.includes('--large');

const jpeg = tinyJpeg();
const mp4 = makeMp4(2);
if (!mp4) {
  console.error('需要系统 ffmpeg 生成测试 MP4');
  process.exit(1);
}

const fat = buildFat32({
  sizeBytes: large ? 64 * 1024 * 1024 : 32 * 1024 * 1024,
  sectorsPerCluster: 8,
  label: 'DASHCAM',
  timezoneOffsetMinutes: 480,
  dirs: [
    { name: 'DCIM', created: at('2026-03-01T00:00:00Z'), modified: at('2026-03-01T00:00:00Z'), accessed: at('2026-03-01T00:00:00Z') },
    { name: 'LOGS', dir: 'DCIM', created: at('2026-03-02T00:00:00Z'), modified: at('2026-03-02T00:00:00Z'), accessed: at('2026-03-02T00:00:00Z') },
  ],
  files: [
    {
      shortName: 'REC_0001.MP4',
      name: '行车记录_20260305_081000.mp4',
      size: mp4.length,
      content: mp4,
      created: at('2026-03-05T00:10:00Z'),
      modified: at('2026-03-05T00:12:30Z'),
      accessed: at('2026-03-05T00:12:30Z'),
    },
    {
      shortName: 'REC_0002.MP4',
      name: '行车记录_20260305_082000.mp4',
      dir: 'DCIM',
      size: 150_000,
      fragment: true,
      created: at('2026-03-05T00:20:00Z'),
      modified: at('2026-03-05T00:23:10Z'),
      accessed: at('2026-03-05T00:23:10Z'),
    },
    {
      shortName: 'IMG_0001.JPG',
      name: '事故现场_01.jpg',
      size: jpeg.length,
      content: jpeg,
      dir: 'DCIM',
      created: at('2026-03-05T00:25:00Z'),
      modified: at('2026-03-05T00:25:00Z'),
      accessed: at('2026-03-05T00:25:00Z'),
    },
    {
      shortName: 'DEL_0001.MP4',
      name: '已删除_保留簇链.mp4',
      size: 120_000,
      deleted: true,
      created: at('2026-03-04T09:00:00Z'),
      modified: at('2026-03-04T09:02:00Z'),
      accessed: at('2026-03-04T09:02:00Z'),
    },
    {
      shortName: 'DEL_0002.MP4',
      name: '已删除_FAT清零.mp4',
      size: 90_000,
      deleted: true,
      clearFatOnDelete: true,
      created: at('2026-03-04T10:00:00Z'),
      modified: at('2026-03-04T10:01:00Z'),
      accessed: at('2026-03-04T10:01:00Z'),
    },
    {
      // 已删除的 JPEG 与 MP4：数据仍留在未分配空间，供雕刻命中。
      shortName: 'GONE_01.JPG',
      name: '已删除_现场照片.jpg',
      size: jpeg.length,
      content: jpeg,
      deleted: true,
      clearFatOnDelete: true,
      created: at('2026-03-04T12:30:00Z'),
      modified: at('2026-03-04T12:30:00Z'),
      accessed: at('2026-03-04T12:30:00Z'),
    },
    {
      shortName: 'GONE_02.MP4',
      name: '已删除_录像片段.mp4',
      size: mp4.length,
      content: mp4,
      deleted: true,
      clearFatOnDelete: true,
      created: at('2026-03-04T12:35:00Z'),
      modified: at('2026-03-04T12:35:00Z'),
      accessed: at('2026-03-04T12:35:00Z'),
    },
    {
      shortName: 'DEL_0003.MP4',
      name: '已删除_部分覆盖.mp4',
      size: 100_000,
      deleted: true,
      overwriteBytesAfterDelete: 40_000,
      created: at('2026-03-04T11:00:00Z'),
      modified: at('2026-03-04T11:01:00Z'),
      accessed: at('2026-03-04T11:01:00Z'),
    },
  ],
  noiseClusters: 400,
});

writeFileSync(join(outDir, 'dashcam-fat32.dd'), fat.bytes);
writeFileSync(
  join(outDir, 'dashcam-fat32.answers.json'),
  JSON.stringify({ type: 'fat32', ...fat.expectations }, null, 2),
);

const exfat = buildExfat({
  sizeBytes: large ? 64 * 1024 * 1024 : 32 * 1024 * 1024,
  clusterSize: 4096,
  label: 'DASHEX',
  timezoneOffsetMinutes: 480,
  dirs: [
    { name: 'DCIM', created: at('2026-03-01T00:00:00Z'), modified: at('2026-03-01T00:00:00Z'), accessed: at('2026-03-01T00:00:00Z') },
  ],
  files: [
    {
      name: '20260305_081000.MP4',
      size: mp4.length,
      content: mp4,
      created: at('2026-03-05T00:10:00Z'),
      modified: at('2026-03-05T00:12:30Z'),
      accessed: at('2026-03-05T00:12:30Z'),
    },
    {
      name: '行车记录仪_20260305_082000.mp4',
      dir: 'DCIM',
      size: 160_000,
      chained: true,
      created: at('2026-03-05T00:20:00Z'),
      modified: at('2026-03-05T00:23:00Z'),
      accessed: at('2026-03-05T00:23:00Z'),
    },
    {
      name: '已删除_连续.mp4',
      size: 80_000,
      deleted: true,
      created: at('2026-03-04T09:00:00Z'),
      modified: at('2026-03-04T09:02:00Z'),
      accessed: at('2026-03-04T09:02:00Z'),
    },
  ],
});
writeFileSync(join(outDir, 'dashcam-exfat.img'), exfat.bytes);
writeFileSync(
  join(outDir, 'dashcam-exfat.answers.json'),
  JSON.stringify({ type: 'exfat', ...exfat.expectations }, null, 2),
);

writeFileSync(join(outDir, 'sample.jpg'), jpeg);
writeFileSync(join(outDir, 'sample.mp4'), mp4);

console.log('测试镜像已生成于 fixtures/：');
console.log(`  dashcam-fat32.dd      ${(fat.bytes.length / 1024 / 1024).toFixed(1)} MiB`);
console.log(`  dashcam-exfat.img     ${(exfat.bytes.length / 1024 / 1024).toFixed(1)} MiB`);
console.log(`  sample.mp4 / sample.jpg`);
