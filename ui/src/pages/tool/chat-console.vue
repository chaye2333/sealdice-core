<template>
  <main class="chat-console-page sd-page-flow">
    <PageHeader title="聊天控制台" />

    <n-grid :cols="isMobile ? 1 : 24" :x-gap="12" :y-gap="12" responsive="screen">
      <n-gi :span="isMobile ? 1 : 7">
        <n-card :bordered="false" class="conv-card">
          <template #header>
            <n-flex align="center" justify="space-between" :wrap="false">
              <n-text strong>会话</n-text>
              <n-button size="tiny" secondary :loading="convLoading" @click="refreshConversations">
                刷新
              </n-button>
            </n-flex>
          </template>

          <ListEmptyState
            v-if="!conversations.length && !convLoading"
            description="暂无会话：先接入官方 QQ 机器人，等群里产生消息"
          />

          <div class="conv-list">
            <div
              v-for="conv in conversations"
              :key="conv.key"
              class="conv-item"
              :class="{ active: conv.key === selectedKey }"
              @click="selectConversation(conv)"
            >
              <n-flex align="center" justify="space-between" :wrap="false">
                <n-text strong class="conv-name">{{ conversationLabel(conv) }}</n-text>
                <n-badge v-if="conv.unread" :value="conv.unread" :max="99" type="error" />
              </n-flex>
              <n-text depth="3" class="conv-last">{{ conv.lastText || '（暂无消息）' }}</n-text>
              <n-flex size="small">
                <n-tag v-if="conv.pinned" size="small" :bordered="false">置顶</n-tag>
                <n-tag size="small" :bordered="false" :type="conv.online ? 'success' : 'default'">
                  {{ conv.online ? '在线' : '离线' }}
                </n-tag>
                <n-tag
                  v-if="conv.kind === 'group' && !conv.fullMessage"
                  size="small"
                  :bordered="false"
                  type="warning"
                >
                  仅 @ 消息
                </n-tag>
              </n-flex>
            </div>
          </div>
        </n-card>
      </n-gi>

      <n-gi :span="isMobile ? 1 : 17">
        <n-card :bordered="false" class="chat-card">
          <template #header>
            <n-flex align="center" justify="space-between" :wrap="false">
              <n-text strong>
                {{ selectedConversation ? conversationLabel(selectedConversation) : '未选择会话' }}
              </n-text>
              <n-text v-if="selectedConversation" depth="3" class="chat-key">
                {{ selectedConversation.key }}
              </n-text>
            </n-flex>
          </template>

          <ListEmptyState v-if="!selectedKey" description="从左侧选择一个会话开始" />

          <template v-else>
            <n-flex justify="center" class="load-more">
              <n-button v-if="hasMore" size="tiny" secondary :loading="loadingMore" @click="loadMore">
                加载更早的消息
              </n-button>
              <n-text v-else-if="messages.length" depth="3">没有更早的消息了</n-text>
            </n-flex>

            <div ref="messagePane" class="msg-scroll">
              <ListEmptyState
                v-if="!messages.length && !msgLoading"
                description="这个会话还没有消息"
              />
              <div
                v-for="item in messages"
                :key="item.id"
                class="msg-row"
                :class="item.dir === 'out' ? 'out' : 'in'"
              >
                <n-text depth="3" class="msg-meta">
                  {{ item.senderName }} · {{ formatChatConsoleTime(item.ts) }}
                </n-text>
                <div class="msg-bubble">
                  <n-text v-if="item.recalled" depth="3" italic>已撤回</n-text>
                  <n-text v-else class="msg-text">{{ item.text }}</n-text>
                </div>
              </div>
            </div>

            <n-flex vertical size="small" class="composer">
              <n-input
                v-model:value="draft"
                type="textarea"
                :autosize="{ minRows: 2, maxRows: 5 }"
                placeholder="输入消息：Enter 发送，Shift+Enter 换行"
                @keydown="onDraftKeydown"
              />
              <n-flex align="center" justify="end" size="small">
                <n-text depth="3" class="composer-hint">小版仅支持文本</n-text>
                <n-button type="primary" :loading="sending" :disabled="!canSend" @click="send">
                  发送
                </n-button>
              </n-flex>
            </n-flex>
          </template>
        </n-card>
      </n-gi>
    </n-grid>
  </main>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue';
import { breakpointsTailwind, useBreakpoints } from '@vueuse/core';
import { useMessage } from 'naive-ui';
import PageHeader from '@/components/shared/PageHeader.vue';
import ListEmptyState from '@/components/shared/ListEmptyState.vue';
import { notifyError } from '@/features/feedback/notifyError';
import {
  fetchChatConsoleConversations,
  fetchChatConsoleMessages,
  markChatConsoleRead,
  sendChatConsoleMessage,
  type ChatConsoleConversation,
  type ChatConsoleMessage,
} from '@/api/chatConsole';
import {
  conversationLabel,
  formatChatConsoleTime,
  mergeChatConsoleMessages,
  patchChatConsoleConversations,
  sortChatConsoleConversations,
} from '@/features/chatConsole/viewModel';

const message = useMessage();
const breakpoints = useBreakpoints(breakpointsTailwind);
const isMobile = breakpoints.smaller('md');

const MESSAGE_PAGE_SIZE = 50;
const CONVERSATION_POLL_MS = 5000;
const MESSAGE_POLL_MS = 3000;

const conversations = ref<ChatConsoleConversation[]>([]);
const convLoading = ref(false);
const selectedKey = ref('');
const messages = ref<ChatConsoleMessage[]>([]);
const hasMore = ref(false);
const msgLoading = ref(false);
const loadingMore = ref(false);
const sending = ref(false);
const draft = ref('');
const messagePane = ref<HTMLElement | null>(null);

let convTimer: number | undefined;
let msgTimer: number | undefined;

const selectedConversation = computed(
  () => conversations.value.find(item => item.key === selectedKey.value) ?? null
);
const canSend = computed(
  () => !!selectedKey.value && draft.value.trim().length > 0 && !sending.value
);

function isNearBottom(): boolean {
  const el = messagePane.value;
  if (!el) return true;
  return el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}

function scrollToBottom() {
  void nextTick(() => {
    const el = messagePane.value;
    if (el) el.scrollTop = el.scrollHeight;
  });
}

async function refreshConversations() {
  convLoading.value = true;
  try {
    const next = await fetchChatConsoleConversations();
    conversations.value = patchChatConsoleConversations(conversations.value, next);
  } catch (error) {
    notifyError(error, message, '加载会话失败');
  } finally {
    convLoading.value = false;
  }
}

async function loadSelectedMessages(before = 0, append = false) {
  const key = selectedKey.value;
  if (!key) return;
  try {
    const page = await fetchChatConsoleMessages(key, before, MESSAGE_PAGE_SIZE);
    if (key !== selectedKey.value) return; // 期间切了会话，丢弃这次结果
    messages.value = append
      ? mergeChatConsoleMessages(page.messages, messages.value)
      : mergeChatConsoleMessages([], page.messages);
    hasMore.value = page.hasMore;
    if (!append) scrollToBottom();
  } catch (error) {
    notifyError(error, message, '加载消息失败');
  }
}

/** 轮询只拉最新一页：合并进现有列表，不打断正在看的历史位置。 */
async function refreshLatestMessages() {
  const key = selectedKey.value;
  if (!key) return;
  const stickToBottom = isNearBottom();
  try {
    const page = await fetchChatConsoleMessages(key, 0, MESSAGE_PAGE_SIZE);
    if (key !== selectedKey.value) return;
    const before = messages.value.length;
    messages.value = mergeChatConsoleMessages(messages.value, page.messages);
    if (messages.value.length !== before) {
      if (stickToBottom) scrollToBottom();
      await markChatConsoleRead(key);
      void refreshConversations();
    }
  } catch {
    // 轮询失败不打扰用户，下一轮再试
  }
}

function startMessagePolling() {
  stopMessagePolling();
  msgTimer = window.setInterval(() => {
    if (!document.hidden) void refreshLatestMessages();
  }, MESSAGE_POLL_MS);
}

function stopMessagePolling() {
  if (msgTimer) {
    window.clearInterval(msgTimer);
    msgTimer = undefined;
  }
}

async function selectConversation(conv: ChatConsoleConversation) {
  if (!conv?.key) return;
  selectedKey.value = conv.key;
  messages.value = [];
  hasMore.value = false;
  msgLoading.value = true;
  startMessagePolling();
  try {
    await loadSelectedMessages();
    await markChatConsoleRead(conv.key);
    await refreshConversations();
  } catch {
    // loadSelectedMessages 里已经提示过
  } finally {
    msgLoading.value = false;
  }
}

async function loadMore() {
  if (loadingMore.value || !messages.value.length) return;
  loadingMore.value = true;
  try {
    await loadSelectedMessages(messages.value[0]?.ts ?? 0, true);
  } finally {
    loadingMore.value = false;
  }
}

async function send() {
  const text = draft.value.trim();
  const key = selectedKey.value;
  if (!text || !key || sending.value) return;
  sending.value = true;
  try {
    const sent = await sendChatConsoleMessage(key, text);
    messages.value = mergeChatConsoleMessages(messages.value, [sent]);
    draft.value = '';
    scrollToBottom();
    void refreshConversations();
  } catch (error) {
    notifyError(error, message, '发送失败');
  } finally {
    sending.value = false;
  }
}

function onDraftKeydown(event: KeyboardEvent) {
  if (event.key !== 'Enter' || event.shiftKey) return;
  event.preventDefault();
  void send();
}

onMounted(async () => {
  conversations.value = sortChatConsoleConversations(await fetchChatConsoleConversations().catch(() => []));
  convTimer = window.setInterval(() => {
    if (!document.hidden) void refreshConversations();
  }, CONVERSATION_POLL_MS);
});

onUnmounted(() => {
  if (convTimer) window.clearInterval(convTimer);
  stopMessagePolling();
});
</script>

<style scoped>
.conv-card,
.chat-card {
  height: 100%;
}

.conv-list {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  max-height: 62vh;
  overflow-y: auto;
}

.conv-item {
  padding: 0.5rem 0.6rem;
  border-radius: 0.5rem;
  cursor: pointer;
  border: 1px solid transparent;
}

.conv-item:hover {
  background: var(--sd-hover-color, rgba(128, 128, 128, 0.08));
}

.conv-item.active {
  border-color: var(--primary-color, #63e2b7);
  background: var(--sd-hover-color, rgba(128, 128, 128, 0.08));
}

.conv-name {
  max-width: 12rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.conv-last {
  display: block;
  margin: 0.25rem 0;
  font-size: 0.85rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.load-more {
  margin-bottom: 0.5rem;
}

.msg-scroll {
  max-height: 52vh;
  overflow-y: auto;
  padding: 0 0.25rem;
}

.msg-row {
  display: flex;
  flex-direction: column;
  margin-bottom: 0.6rem;
}

.msg-row.out {
  align-items: flex-end;
}

.msg-row.in {
  align-items: flex-start;
}

.msg-meta {
  font-size: 0.75rem;
  margin-bottom: 0.15rem;
}

.msg-bubble {
  max-width: min(38rem, 82%);
  padding: 0.4rem 0.6rem;
  border-radius: 0.5rem;
  background: var(--sd-hover-color, rgba(128, 128, 128, 0.12));
}

.msg-row.out .msg-bubble {
  background: var(--primary-color, #63e2b7);
  color: #101014;
}

.msg-text {
  white-space: pre-wrap;
  word-break: break-word;
}

.msg-row.out .msg-text {
  color: inherit;
}

.composer {
  margin-top: 0.5rem;
}

.composer-hint {
  font-size: 0.8rem;
}

.chat-key {
  max-width: 22rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
