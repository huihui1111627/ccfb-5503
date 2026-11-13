/**
 * 监管链服务：把核心哈希链与 IndexedDB 持久化串起来。
 * 每次操作后调用 append 并立即持久化，避免崩溃丢失记录。
 */
import type { CaseRecord, CustodyAction, CustodyEvent } from '../core/types';
import { appendCustodyEvent, verifyCustodyChain } from '../core/custody';
import { store } from './store';

export class CustodyService {
  private events: CustodyEvent[] = [];
  private loadedFor: string | null = null;

  async load(caseRecord: CaseRecord): Promise<CustodyEvent[]> {
    if (this.loadedFor !== caseRecord.id) {
      this.events = await store.listCustody(caseRecord.id);
      this.loadedFor = caseRecord.id;
    }
    return this.events;
  }

  get current(): CustodyEvent[] {
    return this.events;
  }

  async append(
    caseRecord: CaseRecord,
    action: CustodyAction,
    target: string,
    details: Record<string, unknown> = {},
  ): Promise<CustodyEvent> {
    await this.load(caseRecord);
    const event = await appendCustodyEvent(this.events, {
      caseId: caseRecord.id,
      actor: caseRecord.examiner || '未署名',
      action,
      target,
      details,
    });
    this.events.push(event);
    await store.putCustody([event]);
    return event;
  }

  async verify(): Promise<ReturnType<typeof verifyCustodyChain>> {
    return verifyCustodyChain(this.events);
  }

  exportJson(caseRecord: CaseRecord): string {
    return JSON.stringify(
      {
        caseNumber: caseRecord.caseNumber,
        caseId: caseRecord.id,
        exportedAt: new Date().toISOString(),
        note: '哈希链为软件级防篡改，不等同于第三方可信时间戳或数字签名。',
        events: this.events,
      },
      null,
      2,
    );
  }
}

export const custody = new CustodyService();
