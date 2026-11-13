import { client } from './generated/client.gen';

/**
 * 聊天控制台的接口还是 v1（`/sd-api/chat-console/*`，echo 风格），
 * 没有进 OpenAPI，所以这里手工封装一层，复用同一个 axios 实例
 * （鉴权头、401 清理会话、错误归一化都由 `api/client.ts` 的拦截器统一处理）。
 */

export type ChatConsoleKind = 'group' | 'c2c';

export type ChatConsoleConversation = {
  key: string;
  kind: ChatConsoleKind;
  targetId: string;
  endpointId?: string;
  avatarUrl?: string;
  name: string;
  nameManual: boolean;
  memberCount?: number;
  fullMessage: boolean;
  pinned: boolean;
  lastTs: number;
  lastText: string;
  unread: number;
  updatedAt: number;
  online: boolean;
};

export type ChatConsoleMessage = {
  id: string;
  ts: number;
  /** in = 对方发来，out = 骰子发出 */
  dir: 'in' | 'out';
  source: 'user' | 'bot' | 'console';
  senderId: string;
  senderName: string;
  avatarUrl?: string;
  text: string;
  msgId?: string;
  recalled?: boolean;
};

type Envelope = { result?: boolean; err?: string };

async function requestJson<T>(
  method: 'get' | 'post',
  url: string,
  options: { params?: Record<string, unknown>; body?: unknown } = {}
): Promise<T> {
  const response = await client.instance.request<Envelope & T>({
    method,
    url,
    params: options.params,
    data: options.body,
  });
  const data = response.data as (Envelope & T) | undefined;
  if (!data || data.result === false) {
    throw new Error(data?.err || '聊天控制台请求失败');
  }
  return data;
}

export async function fetchChatConsoleConversations(): Promise<ChatConsoleConversation[]> {
  const data = await requestJson<{ conversations?: ChatConsoleConversation[] }>(
    'get',
    '/sd-api/chat-console/conversations'
  );
  return (data.conversations ?? []).filter(item => !!item?.key);
}

export async function fetchChatConsoleMessages(
  key: string,
  before = 0,
  limit = 50
): Promise<{ messages: ChatConsoleMessage[]; hasMore: boolean }> {
  const data = await requestJson<{ messages?: ChatConsoleMessage[]; hasMore?: boolean }>(
    'get',
    '/sd-api/chat-console/messages',
    { params: { key, before, limit } }
  );
  return { messages: data.messages ?? [], hasMore: !!data.hasMore };
}

export async function sendChatConsoleMessage(
  key: string,
  text: string
): Promise<ChatConsoleMessage> {
  const data = await requestJson<{ message?: ChatConsoleMessage }>('post', '/sd-api/chat-console/send', {
    body: { key, text },
  });
  if (!data.message) throw new Error('发送失败：服务端没有返回消息');
  return data.message;
}

export async function markChatConsoleRead(key: string): Promise<void> {
  await requestJson<Record<string, unknown>>('post', '/sd-api/chat-console/read', {
    body: { key },
  });
}
