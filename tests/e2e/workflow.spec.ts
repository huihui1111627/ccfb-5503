/**
 * 端到端：案件 → 导入镜像 → 扫描 → 恢复 → 雕刻 → 时间线 → 报告。
 * 使用真实浏览器与真实 Worker；镜像来自 scripts/make-fixtures.ts 的已知答案文件。
 */
import { test, expect } from '@playwright/test';
import { readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';

const FIXTURE = join(process.cwd(), 'fixtures', 'dashcam-fat32.dd');
const ANSWERS = join(process.cwd(), 'fixtures', 'dashcam-fat32.answers.json');

test.describe.configure({ mode: 'serial' });

test.beforeAll(() => {
  if (!existsSync(FIXTURE)) {
    throw new Error('缺少测试镜像，请先运行 npm run fixtures');
  }
});

test('完整取证流程（FAT32 删除与覆盖）', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  const downloads: string[] = [];
  page.on('download', (d) => downloads.push(d.suggestedFilename()));

  await page.goto('/');
  await page.evaluate(async () => {
    localStorage.clear();
    const dbs = await indexedDB.databases?.();
    await Promise.all((dbs ?? []).map((d) => d.name ? new Promise<void>((res) => {
      const req = indexedDB.deleteDatabase(d.name!);
      req.onsuccess = () => res();
      req.onerror = () => res();
      req.onblocked = () => res();
    }) : Promise.resolve()));
  });
  await page.reload();
  await page.waitForTimeout(800);

  // --- 案件 ---
  await page.getByLabel('案件编号 *').fill('事故鉴 2026-031');
  await page.getByLabel('案件名称 *').fill('某路段行车记录仪存储卡取证');
  await page.getByLabel('送检单位').fill('某事故鉴定所');
  await page.getByLabel('鉴定人').fill('李四');
  await page.getByLabel('案件时区（FAT 时间戳解释依据）').selectOption('480');
  await page.getByRole('button', { name: '创建案件' }).click();
  await expect(page.getByText('事故鉴 2026-031').first()).toBeVisible();
  await expect(page.getByText('哈希链完整', { exact: true }).first()).toBeVisible();

  // --- 分析 ---
  await page.getByRole('link', { name: /镜像分析/ }).click();
  await page.locator('input[type=file]').setInputFiles(FIXTURE);
  await expect(page.getByText('镜像摘要')).toBeVisible({ timeout: 90_000 });
  await expect(page.getByText('DASHCAM').first()).toBeVisible();
  await expect(page.getByText('FAT32').first()).toBeVisible();

  // 核对哈希与答案一致
  const answers = JSON.parse(readFileSync(ANSWERS, 'utf8')) as {
    imageSha256: string;
    imageMd5: string;
    files: Array<{ name: string; sha256: string; deleted: boolean; expectStatus: string }>;
    volume: { totalClusters: number; freeClusters: number };
  };
  const shaCell = page.locator('.metric').filter({ hasText: 'SHA-256' }).locator('strong');
  await expect(shaCell).toHaveText(answers.imageSha256, { timeout: 60_000 });
  const md5Cell = page.locator('.metric').filter({ hasText: 'MD5' }).locator('strong');
  await expect(md5Cell).toHaveText(answers.imageMd5);

  // 复核哈希（只读证明）
  await page.getByRole('button', { name: '复核镜像哈希' }).click();
  await expect(page.getByText('复核一致（只读）')).toBeVisible({ timeout: 90_000 });

  // 扫描
  await page.getByRole('button', { name: '开始扫描' }).click();
  await expect(page.getByText('现存文件')).toBeVisible({ timeout: 90_000 });
  await expect(page.getByText('已删除文件')).toBeVisible();

  // --- 恢复 ---
  await page.getByRole('button', { name: /查看文件与恢复/ }).click();
  await expect(page.getByRole('heading', { name: '文件与恢复' })).toBeVisible();

  // 已删除文件应可筛出
  await page.getByRole('button', { name: '已删除', exact: true }).click();
  const deletedRow = page.locator('tr.deleted').filter({ hasText: '已删除_保留簇链.mp4' });
  await expect(deletedRow).toBeVisible();
  await deletedRow.getByRole('button', { name: '恢复' }).click();
  await expect(page.getByText('完整恢复').first()).toBeVisible({ timeout: 90_000 });

  // 打开详情并核对 SHA-256 与答案一致
  await deletedRow.click();
  const expected = answers.files.find((f) => f.name === '已删除_保留簇链.mp4')!;
  await expect(page.locator('.recovery-result')).toContainText(expected.sha256);
  await expect(page.getByRole('link', { name: '下载恢复文件' })).toBeVisible();
  await page.getByRole('link', { name: '下载恢复文件' }).click();
  await page.waitForTimeout(1000);
  expect(downloads.some((n) => n.includes('已删除_保留簇链'))).toBe(true);

  // 部分覆盖的文件应标记为“部分恢复”或“已覆盖”
  const partialRow = page.locator('tr.deleted').filter({ hasText: '已删除_部分覆盖.mp4' });
  await expect(partialRow).toBeVisible();
  await partialRow.getByRole('button', { name: '恢复' }).click();
  await expect(
    page.locator('tr.deleted').filter({ hasText: '已删除_部分覆盖.mp4' }).getByText(/部分恢复|已覆盖/),
  ).toBeVisible({ timeout: 60_000 });

  // 长文件名恢复标记
  await expect(page.getByText('长名已恢复').first()).toBeVisible();

  // --- 雕刻 ---
  await page.getByRole('button', { name: /雕刻未分配空间/ }).click();
  await page.getByRole('button', { name: '开始雕刻' }).click();
  await expect(page.getByText(/MP4|JPEG/).first()).toBeVisible({ timeout: 120_000 });

  // --- 时间线 ---
  await page.getByRole('link', { name: /时间线/ }).click();
  await page.getByRole('button', { name: '构建时间线' }).click();
  await expect(page.getByText('事件列表')).toBeVisible({ timeout: 60_000 });
  await expect(page.locator('tbody tr').first()).toBeVisible();
  // 时间线应显示“按案件时区”
  await expect(page.getByText('按案件时区').first()).toBeVisible();

  // --- 报告 ---
  await page.getByRole('link', { name: /报告/ }).click();
  await expect(page.getByText('报告预览')).toBeVisible();
  const pdfPromise = page.waitForEvent('download');
  await page.getByRole('button', { name: '生成 PDF 报告' }).click();
  const pdf = await pdfPromise;
  expect(pdf.suggestedFilename()).toMatch(/\.pdf$/);
  const path = await pdf.path();
  expect(path).not.toBeNull();
  const head = readFileSync(path!);
  expect(head.subarray(0, 5).toString()).toBe('%PDF-');

  // 监管链完整
  await expect(page.getByText('监管链完整', { exact: true })).toBeVisible();

  expect(errors, `浏览器错误: ${errors.join('\n')}`).toEqual([]);
});
