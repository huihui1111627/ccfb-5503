import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  worker: { format: 'es' },
  build: {
    target: 'es2022',
    sourcemap: true,
    chunkSizeWarningLimit: 1024,
    rollupOptions: {
      output: {
        // pdf-lib 体积大但只在“生成报告”时使用，单独分包避免拖慢首屏。
        manualChunks: {
          'pdf-engine': ['pdf-lib', '@pdf-lib/fontkit'],
          react: ['react', 'react-dom', 'react-router-dom'],
        },
      },
    },
  },
  server: { port: 5173, strictPort: true },
  test: {
    include: ['tests/core/**/*.test.ts'],
    environment: 'node',
    testTimeout: 120_000,
    hookTimeout: 120_000,
  },
});
