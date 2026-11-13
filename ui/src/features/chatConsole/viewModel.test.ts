import { describe, expect, it } from 'vitest';
import type { ChatConsoleConversation, ChatConsoleMessage } from '@/api/chatConsole';
import {
  conversationLabel,
  mergeChatConsoleMessages,
  patchChatConsoleConversations,
  sortChatConsoleConversations,
} from './viewModel';

function conv(partial: Partial<ChatConsoleConversation> & { key: string }): ChatConsoleConversation {
  return {
    kind: 'group',
    targetId: partial.key,
    name: '',
    nameManual: false,
    fullMessage: false,
    pinned: false,
    lastTs: 0,
    lastText: '',
    unread: 0,
    updatedAt: 0,
    online: true,
    ...partial,
  };
}

function msg(partial: Partial<ChatConsoleMessage> & { id: string }): ChatConsoleMessage {
  return {
    ts: 0,
    dir: 'in',
    source: 'user',
    senderId: 'u',
    senderName: '某人',
    text: '',
    ...partial,
  };
}

describe('聊天控制台视图模型', () => {
  it('会话名缺失时退化成带类型的目标尾号', () => {
    expect(conversationLabel(conv({ key: 'group:1', name: '测试群' }))).toBe('测试群');
    expect(conversationLabel(conv({ key: 'group:abcdef123456', name: '  ', targetId: 'abcdef123456' }))).toBe(
      '群 123456'
    );
    expect(conversationLabel(conv({ key: 'c2c:9', kind: 'c2c', name: '', targetId: 'openid9' }))).toBe(
      '用户 openid9'
    );
    expect(conversationLabel(null)).toBe('');
  });

  it('消息按 id 去重并按时间升序合并', () => {
    const existing = [msg({ id: 'b', ts: 2 }), msg({ id: 'a', ts: 1 })];
    const merged = mergeChatConsoleMessages(existing, [
      msg({ id: 'a', ts: 1 }), // 轮询重复
      msg({ id: 'c', ts: 3 }),
      msg({ id: '', ts: 4 }), // 没有 id 的脏数据直接丢
    ]);
    expect(merged.map(item => item.id)).toEqual(['a', 'b', 'c']);
    expect(existing.map(item => item.id)).toEqual(['b', 'a']); // 不改入参
  });

  it('会话排序：置顶优先，其次按最后消息时间倒序', () => {
    const sorted = sortChatConsoleConversations([
      conv({ key: 'old', lastTs: 10 }),
      conv({ key: 'pinned-old', lastTs: 5, pinned: true }),
      conv({ key: 'new', lastTs: 20 }),
    ]);
    expect(sorted.map(item => item.key)).toEqual(['pinned-old', 'new', 'old']);
  });

  it('轮询刷新时保留本地未读数', () => {
    const patched = patchChatConsoleConversations(
      [conv({ key: 'g1', unread: 3 }), conv({ key: 'g2', unread: 0 })],
      [conv({ key: 'g1', unread: 0 }), conv({ key: 'g2', unread: 0 })]
    );
    expect(patched.find(item => item.key === 'g1')?.unread).toBe(3);
    expect(patched.find(item => item.key === 'g2')?.unread).toBe(0);
  });
});
