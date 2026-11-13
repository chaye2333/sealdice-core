import dayjs from 'dayjs';
import type { ChatConsoleConversation, ChatConsoleMessage } from '@/api/chatConsole';

/** 会话显示名：没抓到名字时退化成「群/用户 + targetId 尾号」，不要显示空白。 */
export function conversationLabel(conv: ChatConsoleConversation | null | undefined): string {
  if (!conv) return '';
  const name = conv.name?.trim();
  if (name) return name;
  const raw = conv.targetId || conv.key;
  // openid 很长，只要尾号够辨认；短 ID（如测试里的 openid9）就原样展示。
  const suffix = raw.length > 8 ? raw.slice(-6) : raw;
  return `${conv.kind === 'group' ? '群' : '用户'} ${suffix}`;
}

export function formatChatConsoleTime(ts: number): string {
  if (!ts) return '';
  const time = dayjs.unix(ts);
  return time.isSame(dayjs(), 'day') ? time.format('HH:mm') : time.format('MM-DD HH:mm');
}

export function formatChatConsoleClock(ts: number): string {
  if (!ts) return '-';
  return dayjs.unix(ts).format('YYYY-MM-DD HH:mm:ss');
}

/**
 * 合并消息：按 id 去重（轮询会重复拿到同一批），按时间升序。
 * 逐条 push 而不是整批替换，避免正在看历史时被新消息把滚动位置顶掉。
 */
export function mergeChatConsoleMessages(
  existing: readonly ChatConsoleMessage[],
  incoming: readonly ChatConsoleMessage[]
): ChatConsoleMessage[] {
  const seen = new Set(existing.map(item => item.id));
  const merged = [...existing];
  for (const item of incoming) {
    if (!item?.id || seen.has(item.id)) continue;
    seen.add(item.id);
    merged.push(item);
  }
  return merged.sort((a, b) => a.ts - b.ts || a.id.localeCompare(b.id));
}

/** 会话列表排序：置顶优先，其次按最后消息时间倒序。 */
export function sortChatConsoleConversations(
  conversations: readonly ChatConsoleConversation[]
): ChatConsoleConversation[] {
  return [...conversations].sort((a, b) => {
    if (a.pinned !== b.pinned) return a.pinned ? -1 : 1;
    return (b.lastTs || b.updatedAt || 0) - (a.lastTs || a.updatedAt || 0);
  });
}

/** 会话列表轮询时保留本地选中态与未读数（服务端未读为 0 时不覆盖本地）。 */
export function patchChatConsoleConversations(
  previous: readonly ChatConsoleConversation[],
  next: readonly ChatConsoleConversation[]
): ChatConsoleConversation[] {
  const previousByKey = new Map(previous.map(item => [item.key, item]));
  return sortChatConsoleConversations(
    next.map(item => {
      const old = previousByKey.get(item.key);
      if (old && item.unread === 0 && old.unread > 0) return { ...item, unread: old.unread };
      return item;
    })
  );
}
