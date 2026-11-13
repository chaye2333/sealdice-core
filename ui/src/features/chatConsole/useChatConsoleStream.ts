import { onUnmounted, ref } from 'vue';
import { getApiBaseUrl } from '@/api/config';
import type { ChatConsoleEvent } from '@/api/chatConsole';
import { useAuthStore } from '@/features/auth/store';
import { appPinia } from '@/pinia';

export type ChatConsoleStreamState = 'idle' | 'connecting' | 'connected' | 'disconnected';

const INITIAL_RETRY_MS = 1000;
const MAX_RETRY_MS = 10000;

/** 解析一个 SSE 事件块（`event:` 与 `data:` 行，忽略 `:` 心跳注释）。 */
export function parseChatConsoleSSEBlock(block: string): { event: string; data: string } {
  let event = '';
  const data: string[] = [];
  for (const rawLine of block.split('\n')) {
    const line = rawLine.replace(/\r$/, '');
    if (!line || line.startsWith(':')) continue;
    const sep = line.indexOf(':');
    const field = sep === -1 ? line : line.slice(0, sep);
    let value = sep === -1 ? '' : line.slice(sep + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'event') event = value;
    else if (field === 'data') data.push(value);
  }
  return { event, data: data.join('\n') };
}

/**
 * 订阅 `/sd-api/chat-console/events`。
 *
 * 不用 `EventSource`：它无法携带 `Authorization` 头，而这里需要 `token` 头。
 * 所以用 fetch + ReadableStream 自己按 SSE 格式切块；断线按 1s→2s→…→10s 退避重连。
 */
export function useChatConsoleStream(options: {
  onEvent: (event: ChatConsoleEvent) => void;
  onStateChange?: (state: ChatConsoleStreamState) => void;
}) {
  const state = ref<ChatConsoleStreamState>('idle');
  let controller: AbortController | undefined;
  let retryTimer: number | undefined;
  let retryDelay = INITIAL_RETRY_MS;
  let stopped = false;
  let generation = 0;

  function setState(next: ChatConsoleStreamState) {
    if (state.value === next) return;
    state.value = next;
    options.onStateChange?.(next);
  }

  function clearRetry() {
    if (retryTimer) {
      window.clearTimeout(retryTimer);
      retryTimer = undefined;
    }
  }

  function scheduleReconnect() {
    if (stopped) return;
    setState('disconnected');
    clearRetry();
    retryTimer = window.setTimeout(() => {
      retryTimer = undefined;
      void connect();
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, MAX_RETRY_MS);
  }

  function handleBlock(block: string) {
    const { event, data } = parseChatConsoleSSEBlock(block);
    if (event === 'ready') {
      setState('connected');
      retryDelay = INITIAL_RETRY_MS;
      return;
    }
    if (!data) return;
    try {
      const payload = JSON.parse(data) as ChatConsoleEvent;
      if (!payload || typeof payload !== 'object') return;
      options.onEvent({ ...payload, type: payload.type || event });
    } catch {
      // 协议里出现坏帧就跳过，别把整条流打断
    }
  }

  async function connect() {
    if (stopped) return;
    const token = useAuthStore(appPinia).token;
    if (!token) {
      scheduleReconnect();
      return;
    }

    const current = ++generation;
    controller?.abort();
    controller = new AbortController();
    setState('connecting');

    try {
      const url = `${getApiBaseUrl()}/sd-api/chat-console/events?token=${encodeURIComponent(token)}`;
      const response = await fetch(url, {
        headers: { token, Accept: 'text/event-stream' },
        credentials: 'include',
        signal: controller.signal,
      });
      if (current !== generation) return;
      if (!response.ok || !response.body) {
        scheduleReconnect();
        return;
      }

      setState('connected');
      retryDelay = INITIAL_RETRY_MS;

      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';

      for (;;) {
        const { value, done } = await reader.read();
        if (current !== generation) return;
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let index = buffer.indexOf('\n\n');
        while (index >= 0) {
          const block = buffer.slice(0, index);
          buffer = buffer.slice(index + 2);
          if (block.trim()) handleBlock(block);
          index = buffer.indexOf('\n\n');
        }
      }
      // 服务端主动收流（例如重启）→ 走退避重连
      scheduleReconnect();
    } catch (error) {
      if ((error as Error)?.name === 'AbortError') return;
      scheduleReconnect();
    }
  }

  function start() {
    stopped = false;
    retryDelay = INITIAL_RETRY_MS;
    void connect();
  }

  function stop() {
    stopped = true;
    generation += 1;
    clearRetry();
    controller?.abort();
    controller = undefined;
    setState('idle');
  }

  function restart() {
    stop();
    start();
  }

  onUnmounted(stop);

  return { state, start, stop, restart };
}
