import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 180_000,
  expect: { timeout: 30_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [['list']],
  // 上次测试的 IndexedDB 会留在浏览器里，导致“本机案件”列表累积、断言误命中。
  // 每个测试使用独立上下文并清空存储。
  use: {
    baseURL: 'http://localhost:5173',
    trace: 'retain-on-failure',
    video: 'off',
    storageState: { cookies: [], origins: [] },
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],
  webServer: {
    command: 'npm run dev',
    url: 'http://localhost:5173',
    reuseExistingServer: true,
    timeout: 60_000,
  },
});
