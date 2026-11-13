/**
 * exFAT 端到端：导入 → 扫描 → 恢复已删除连续文件 → 时间线偏移正确。
 */
import { test, expect } from '@playwright/test';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const FIXTURE = join(process.cwd(), 'fixtures', 'dashcam-exfat.img');
const ANSWERS = join(process.cwd(), 'fixtures', 'dashcam-exfat.answers.json');

test.beforeAll(() => {
  if (!existsSync(FIXTURE)) throw new Error('缺少 exFAT 测试镜像，请先运行 npm run fixtures');
});

test('exFAT 卷扫描与删除恢复', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });

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
  await page.getByLabel('案件编号 *').fill('事故鉴 2026-032');
  await page.getByLabel('案件名称 *').fill('exFAT 存储卡取证');
  await page.getByLabel('鉴定人').fill('李四');
  await page.getByRole('button', { name: '创建案件' }).click();

  await page.getByRole('link', { name: /镜像分析/ }).click();
  await page.locator('input[type=file]').setInputFiles(FIXTURE);
  await expect(page.getByText('exFAT').first()).toBeVisible({ timeout: 90_000 });
  await expect(page.getByText('DASHEX').first()).toBeVisible();

  const answers = JSON.parse(readFileSync(ANSWERS, 'utf8')) as {
    imageSha256: string;
    files: Array<{ name: string; sha256: string; deleted: boolean }>;
  };
  const shaCell = page.locator('.metric').filter({ hasText: 'SHA-256' }).locator('strong');
  await expect(shaCell).toHaveText(answers.imageSha256, { timeout: 60_000 });

  await page.getByRole('button', { name: '开始扫描' }).click();
  await expect(page.getByText('现存文件')).toBeVisible({ timeout: 90_000 });

  await page.getByRole('button', { name: /查看文件与恢复/ }).click();
  await page.getByRole('button', { name: '已删除', exact: true }).click();
  const row = page.locator('tr.deleted').filter({ hasText: '已删除_连续.mp4' });
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: '恢复' }).click();
  await row.click();
  const expected = answers.files.find((f) => f.name === '已删除_连续.mp4')!;
  await expect(page.locator('.recovery-result')).toContainText(expected.sha256, { timeout: 60_000 });

  // 时间线：exFAT 时间戳应显示“磁盘显式偏移”
  await page.getByRole('link', { name: /时间线/ }).click();
  await page.getByRole('button', { name: '构建时间线' }).click();
  await expect(page.getByText('事件列表')).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText('磁盘显式偏移').first()).toBeVisible();

  expect(errors, errors.join('\n')).toEqual([]);
});
