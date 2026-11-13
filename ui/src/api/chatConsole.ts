import { useAuthStore } from '@/features/auth/store';
import { appPinia } from '@/pinia';
import { client } from './generated/client.gen';

/**
 * 聊天控制台的接口还是 v1（`/sd-api/chat-console/*`，echo 风格），没有进 OpenAPI，
 * 所以这里手工封装，复用同一个 axios 实例（401 清理、错误归一化都走既有拦截器）。
 *
 * ⚠️ 鉴权注意：v1 的 `doAuth` 读的是 **`token` 头**（或 `?token=` 查询参数），
 * 不是 `Authorization: Bearer`。所以这里两个头都带上：前者给 v1，后者给统一拦截器。
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
  fingerMemo?: string;
  classText?: string;
  tags?: string[];
  fullMessage: boolean;
  pinned: boolean;
  lastTs: number;
  lastText: string;
  unread: number;
  updatedAt: number;
  online: boolean;
};

export type ChatConsoleMedia = {
  /** image / record / video / file */
  kind: string;
  url: string;
  name?: string;
};

/** 结构化卡片（message_type=3 的 ark_data），前端渲染成可点的链接卡片。 */
export type ChatConsoleCard = {
  arkName?: string;
  arkType?: string;
  title?: string;
  desc?: string;
  source?: string;
  url?: string;
  preview?: string;
};

export type ChatConsoleQuote = {
  sender: string;
  text: string;
  msgId?: string;
};

export type ChatConsoleMessage = {
  id: string;
  ts: number;
  /** in = 对方发来，out = 骰子发出 */
  dir: 'in' | 'out';
  source: 'user' | 'bot' | 'console';
  senderId: string;
  senderName: string;
  senderOpenId?: string;
  avatarUrl?: string;
  text: string;
  media?: ChatConsoleMedia[];
  card?: ChatConsoleCard;
  msgId?: string;
  /** message_scene.ext 的 REFIDX_…，引用用户消息时后端会优先用它 */
  msgIdx?: string;
  quote?: ChatConsoleQuote;
  recalled?: boolean;
};

export type ChatConsoleSearchMessage = ChatConsoleMessage & {
  conversationKey: string;
  conversationName: string;
};

export type ChatConsoleEvent = {
  /** message / recall / conversation / settings */
  type: string;
  conversation?: ChatConsoleConversation;
  message?: ChatConsoleMessage;
};

type Envelope = { result?: boolean; err?: string };

function authHeaders(): Record<string, string> {
  const token = useAuthStore(appPinia).token;
  if (!token) return {};
  // v1 认 token 头；Authorization 让统一拦截器把它当成正常登录请求处理
  return { token, Authorization: `Bearer ${token}` };
}

async function requestJson<T>(
  method: 'get' | 'post',
  url: string,
  options: { params?: Record<string, unknown>; body?: unknown; formData?: FormData } = {}
): Promise<T> {
  const response = await client.instance.request<Envelope & T>({
    method,
    url,
    params: options.params,
    data: options.formData ?? options.body,
    headers: authHeaders(),
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
  text: string,
  quote?: ChatConsoleQuote
): Promise<ChatConsoleMessage> {
  const data = await requestJson<{ message?: ChatConsoleMessage }>('post', '/sd-api/chat-console/send', {
    body: { key, text, quote: quote ?? null },
  });
  if (!data.message) throw new Error('发送失败：服务端没有返回消息');
  return data.message;
}

export async function uploadChatConsoleFile(
  key: string,
  file: File
): Promise<ChatConsoleMessage> {
  const formData = new FormData();
  formData.append('key', key);
  formData.append('file', file, file.name);
  const data = await requestJson<{ message?: ChatConsoleMessage }>(
    'post',
    '/sd-api/chat-console/upload',
    { formData }
  );
  if (!data.message) throw new Error('发送失败：服务端没有返回消息');
  return data.message;
}

export async function markChatConsoleRead(key: string): Promise<void> {
  await requestJson<Record<string, unknown>>('post', '/sd-api/chat-console/read', {
    body: { key },
  });
}

export async function pinChatConsoleConversation(
  key: string,
  pinned: boolean
): Promise<ChatConsoleConversation | undefined> {
  const data = await requestJson<{ conversation?: ChatConsoleConversation }>(
    'post',
    '/sd-api/chat-console/pin',
    { body: { key, pinned } }
  );
  return data.conversation;
}

export async function renameChatConsoleConversation(
  key: string,
  name: string
): Promise<ChatConsoleConversation | undefined> {
  const data = await requestJson<{ conversation?: ChatConsoleConversation }>(
    'post',
    '/sd-api/chat-console/rename',
    { body: { key, name } }
  );
  return data.conversation;
}

export async function refreshChatConsoleGroupInfo(
  key: string
): Promise<ChatConsoleConversation | undefined> {
  const data = await requestJson<{ conversation?: ChatConsoleConversation }>(
    'post',
    '/sd-api/chat-console/group-info',
    { body: { key } }
  );
  return data.conversation;
}

export async function searchChatConsole(
  query: string,
  limit = 30
): Promise<{ conversations: ChatConsoleConversation[]; messages: ChatConsoleSearchMessage[] }> {
  const data = await requestJson<{
    conversations?: ChatConsoleConversation[];
    messages?: ChatConsoleSearchMessage[];
  }>('get', '/sd-api/chat-console/search', { params: { q: query, limit } });
  return { conversations: data.conversations ?? [], messages: data.messages ?? [] };
}

export async function recallChatConsoleMessage(
  key: string,
  msgId: string
): Promise<ChatConsoleMessage | undefined> {
  const data = await requestJson<{ message?: ChatConsoleMessage }>(
    'post',
    '/sd-api/chat-console/recall',
    { body: { key, msgId } }
  );
  return data.message;
}

/** seconds <= 0 表示解除禁言。 */
export async function muteChatConsoleMember(
  key: string,
  memberOpenId: string,
  seconds: number
): Promise<void> {
  await requestJson<{ text?: string }>('post', '/sd-api/chat-console/mute', {
    body: { key, memberOpenId, seconds },
  });
}
