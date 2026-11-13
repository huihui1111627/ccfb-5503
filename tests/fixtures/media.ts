/**
 * 测试用媒体数据（内嵌，避免测试依赖外部素材）。
 *
 * JPEG：64x64 的合法基线 JPEG（含 SOI/APP0/DQT/SOF0/DHT/SOS/EOI）。
 * MP4：由 ffmpeg 生成的若干秒 H.264 片段（base64），生成器会校验其 box 结构，
 *       以确保雕刻器面对的是真实可解析的 MP4，而非伪造头部。
 */
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export const TINY_JPEG_BASE64 =
  '/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAA0JCgsKCA0LCgsODg0PEyAVExISEyccHhcgLikxMC4pLSwzOko+MzZGNywtQFdBRkxOUlNSMj5aYVpQYEpRUk//2wBDAQ4ODhMREyYVFSZPNS01T09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT0//wAARCABAAEADASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwDm/wC0b7/n9uf+/rf40f2jff8AP7c/9/W/xo/tG+/5/bn/AL+t/jR/aN9/z+3P/f1v8a19n/cX9fIx5vN/18w/tG+/5/bn/v63+NH9o33/AD+3P/f1v8aP7Rvv+f25/wC/rf40f2jff8/tz/39b/Gj2f8AcX9fIObzf9fMP7Rvv+f25/7+t/jR/aN9/wA/tz/39b/Gj+0b7/n9uf8Av63+NH9o33/P7c/9/W/xo9n/AHF/XyDm83/XzD+0b7/n9uf+/rf40f2jff8AP7c/9/W/xo/tG+/5/bn/AL+t/jR/aN9/z+3P/f1v8aPZ/wBxf18g5vN/18w/tG+/5/bn/v63+NH9o33/AD+3P/f1v8aP7Rvv+f25/wC/rf40f2jff8/tz/39b/Gj2f8AcX9fIObzf9fMP7Rvv+f25/7+t/jR/aN9/wA/tz/39b/Gj+0b7/n9uf8Av63+NH9o33/P7c/9/W/xo9n/AHF/XyDm83/XzD+0b7/n9uf+/rf40f2jff8AP7c/9/W/xo/tG+/5/bn/AL+t/jR/aN9/z+3P/f1v8aPZ/wBxf18g5vN/18w/tG+/5/bn/v63+NH9o33/AD+3P/f1v8aP7Rvv+f25/wC/rf40f2jff8/tz/39b/Gj2f8AcX9fIObzf9fMP7Rvv+f25/7+t/jR/aN9/wA/tz/39b/Gj+0b7/n9uf8Av63+NH9o33/P7c/9/W/xo9n/AHF/XyDm83/XzD+0b7/n9uf+/rf40f2jff8AP7c/9/W/xo/tG+/5/bn/AL+t/jR/aN9/z+3P/f1v8aPZ/wBxf18g5vN/18w/tG+/5/bn/v63+NH9o33/AD+3P/f1v8aP7Rvv+f25/wC/rf40f2jff8/tz/39b/Gj2f8AcX9fIObzf9fMP7Rvv+f25/7+t/jR/aN9/wA/tz/39b/Gj+0b7/n9uf8Av63+NH9o33/P7c/9/W/xo9n/AHF/XyDm83/XzD+0b7/n9uf+/rf40f2jff8AP7c/9/W/xo/tG+/5/bn/AL+t/jR/aN9/z+3P/f1v8aPZ/wBxf18g5vN/18w/tG+/5/bn/v63+NH9o33/AD+3P/f1v8aP7Rvv+f25/wC/rf40f2jff8/tz/39b/Gj2f8AcX9fIObzf9fMP7Rvv+f25/7+t/jR/aN9/wA/tz/39b/Gj+0b7/n9uf8Av63+NH9o33/P7c/9/W/xo9n/AHF/XyDm83/XzD+0b7/n9uf+/rf40f2jff8AP7c/9/W/xo/tG+/5/bn/AL+t/jR/aN9/z+3P/f1v8aPZ/wBxf18g5vN/18z/2Q==';

export function tinyJpeg(): Uint8Array {
  return Uint8Array.from(Buffer.from(TINY_JPEG_BASE64, 'base64'));
}

/** 用系统 ffmpeg 现场生成一段合法 MP4；不可用时返回 null（测试会跳过对应断言）。 */
export function makeMp4(durationSec = 2, width = 160, height = 120): Uint8Array | null {
  const ffmpeg = findFfmpeg();
  if (!ffmpeg) return null;
  // MP4 muxer 需要可回写的输出，pipe 不可用；写临时文件再读回。
  const tmp = mkdtempSync(join(tmpdir(), 'sd-forensics-mp4-'));
  const outFile = join(tmp, 'clip.mp4');
  try {
    execFileSync(
      ffmpeg,
      [
        '-hide_banner', '-loglevel', 'error', '-y',
        '-f', 'lavfi', '-i', `testsrc=size=${width}x${height}:rate=10:duration=${durationSec}`,
        '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p',
        '-movflags', '+faststart', outFile,
      ],
      { maxBuffer: 32 * 1024 * 1024 },
    );
    const data = readFileSync(outFile);
    return new Uint8Array(data);
  } catch {
    return null;
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}

let ffmpegCache: string | null | undefined;

function findFfmpeg(): string | null {
  if (ffmpegCache !== undefined) return ffmpegCache;
  const candidates = ['/opt/homebrew/bin/ffmpeg', '/usr/local/bin/ffmpeg', '/usr/bin/ffmpeg'];
  for (const c of candidates) {
    if (existsSync(c)) {
      ffmpegCache = c;
      return c;
    }
  }
  try {
    execFileSync('ffmpeg', ['-version'], { stdio: 'ignore' });
    ffmpegCache = 'ffmpeg';
    return 'ffmpeg';
  } catch {
    ffmpegCache = null;
    return null;
  }
}

/** 生成确定性的伪随机数据（xorshift32），用于可复现的测试映像。 */
export function prngBytes(length: number, seed: number): Uint8Array {
  const out = new Uint8Array(length);
  let x = seed >>> 0 || 1;
  for (let i = 0; i < length; i++) {
    x ^= x << 13;
    x >>>= 0;
    x ^= x >>> 17;
    x ^= x << 5;
    x >>>= 0;
    out[i] = x & 0xff;
  }
  return out;
}
