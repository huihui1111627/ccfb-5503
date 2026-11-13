/**
 * 监管链（Chain of Custody）。
 *
 * 每条记录包含前一条记录的哈希，形成可验证的哈希链：
 *   hash = SHA-256(canonicalJSON(event 去掉 hash))
 * 导出 JSON 时可整体复核；任何一条被改动，其后所有哈希都会失配。
 *
 * 说明：这是软件层面的防篡改，不等同于签名/时间戳服务；报告会如实标注。
 */
import type { CustodyAction, CustodyEvent } from './types';
import { sha256Bytes } from '../lib/sha256-utf8';

export const ACTION_LABELS: Record<CustodyAction, string> = {
  'case.create': '创建案件',
  'case.update': '修改案件信息',
  'image.import': '导入镜像',
  'image.hash': '计算镜像哈希',
  'image.verify': '复核镜像哈希',
  'volume.detect': '识别卷与分区',
  'file.scan': '扫描目录项',
  'file.recover': '恢复文件',
  'carve.scan': '未分配空间雕刻',
  'timeline.build': '构建时间线',
  'report.generate': '生成报告',
  'custody.export': '导出监管链',
  'custody.verify': '验证监管链',
};

export interface CustodyAppendInput {
  caseId: string;
  actor: string;
  action: CustodyAction;
  target: string;
  details?: Record<string, unknown>;
  /** 测试可注入固定时间。 */
  at?: string;
}

/** 稳定序列化：键按字典序，保证同一内容跨平台同哈希。 */
export function canonicalJson(value: unknown): string {
  return JSON.stringify(value, (_key, v) => {
    if (v && typeof v === 'object' && !Array.isArray(v)) {
      const obj = v as Record<string, unknown>;
      const sorted: Record<string, unknown> = {};
      for (const k of Object.keys(obj).sort()) sorted[k] = obj[k];
      return sorted;
    }
    return v;
  });
}

export async function appendCustodyEvent(
  events: CustodyEvent[],
  input: CustodyAppendInput,
): Promise<CustodyEvent> {
  const prev = events[events.length - 1];
  const seq = (prev?.seq ?? 0) + 1;
  const at = input.at ?? new Date().toISOString();
  const base: Omit<CustodyEvent, 'hash'> = {
    seq,
    caseId: input.caseId,
    at,
    actor: input.actor,
    action: input.action,
    actionLabel: ACTION_LABELS[input.action],
    target: input.target,
    details: input.details ?? {},
    prevHash: prev?.hash ?? 'GENESIS',
  };
  const hash = await hashEvent(base);
  return { ...base, hash };
}

export function hashEvent(event: Omit<CustodyEvent, 'hash'> | CustodyEvent): Promise<string> {
  const { hash: _ignored, ...rest } = event as CustodyEvent;
  void _ignored;
  return sha256Bytes(new TextEncoder().encode(canonicalJson(rest)));
}

export interface CustodyVerification {
  valid: boolean;
  checked: number;
  brokenAt: number | null;
  reason: string | null;
}

export async function verifyCustodyChain(events: CustodyEvent[]): Promise<CustodyVerification> {
  let prevHash = 'GENESIS';
  for (let i = 0; i < events.length; i++) {
    const e = events[i]!;
    if (e.seq !== i + 1) {
      return { valid: false, checked: i, brokenAt: e.seq, reason: `序号不连续：期望 ${i + 1}，实际 ${e.seq}` };
    }
    if (e.prevHash !== prevHash) {
      return { valid: false, checked: i, brokenAt: e.seq, reason: '前序哈希与实际链不一致' };
    }
    const recomputed = await hashEvent(e);
    if (recomputed !== e.hash) {
      return { valid: false, checked: i, brokenAt: e.seq, reason: '事件内容与记录的哈希不符（可能被修改）' };
    }
    prevHash = e.hash;
  }
  return { valid: true, checked: events.length, brokenAt: null, reason: null };
}
