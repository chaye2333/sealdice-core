<template>
  <main class="chat-console-page sd-page-flow">
    <PageHeader title="聊天控制台" />

    <n-grid :cols="isMobile ? 1 : 24" :x-gap="12" :y-gap="12" responsive="screen">
      <n-gi :span="isMobile ? 1 : 7">
        <n-card :bordered="false" class="conv-card">
          <template #header>
            <n-flex align="center" justify="space-between" :wrap="false">
              <n-flex align="center" size="small">
                <n-text strong>会话</n-text>
                <n-tag size="small" :bordered="false" :type="streamTag.type">
                  {{ streamTag.text }}
                </n-tag>
              </n-flex>
              <n-button size="tiny" secondary :loading="convLoading" @click="refreshConversations">
                刷新
              </n-button>
            </n-flex>
          </template>

          <n-input
            v-model:value="searchQuery"
            size="small"
            clearable
            placeholder="搜索会话 / 消息"
            class="conv-search"
          >
            <template #prefix>
              <i-tabler-search />
            </template>
          </n-input>

          <n-spin :show="searching">
            <template v-if="searchQuery.trim()">
              <n-text depth="3" class="search-hint">
                会话 {{ searchResult.conversations.length }} 个 · 消息
                {{ searchResult.messages.length }} 条
              </n-text>
              <div class="conv-list">
                <div
                  v-for="conv in searchResult.conversations"
                  :key="`c-${conv.key}`"
                  class="conv-item"
                  @click="selectConversation(conv)"
                >
                  <n-text strong>{{ conversationLabel(conv) }}</n-text>
                  <n-text depth="3" class="conv-last">{{ conv.key }}</n-text>
                </div>
                <div
                  v-for="item in searchResult.messages"
                  :key="`m-${item.id}`"
                  class="conv-item"
                  @click="openSearchMessage(item)"
                >
                  <n-text strong>{{ item.conversationName || item.conversationKey }}</n-text>
                  <n-text depth="3" class="conv-last">
                    {{ item.senderName }}：{{ item.text }}
                  </n-text>
                </div>
                <n-text v-if="!searchResult.conversations.length && !searchResult.messages.length" depth="3">
                  没有匹配结果
                </n-text>
              </div>
            </template>

            <template v-else>
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
                    <n-flex align="center" size="small" :wrap="false">
                      <n-avatar
                        round
                        :size="26"
                        :src="avatarSrc(conv.avatarUrl)"
                      >
                        {{ chatConsoleAvatarFallback(conversationLabel(conv)) }}
                      </n-avatar>
                      <n-text strong class="conv-name">{{ conversationLabel(conv) }}</n-text>
                    </n-flex>
                    <n-flex align="center" size="small" :wrap="false">
                      <n-badge v-if="conv.unread" :value="conv.unread" :max="99" type="error" />
                      <n-button
                        size="tiny"
                        quaternary
                        :title="conv.pinned ? '取消置顶' : '置顶'"
                        @click.stop="togglePin(conv)"
                      >
                        <template #icon>
                          <i-tabler-pin />
                        </template>
                      </n-button>
                      <n-dropdown
                        trigger="click"
                        :options="convMenuOptions"
                        @select="key => onConvMenu(key, conv)"
                      >
                        <n-button size="tiny" quaternary @click.stop>
                          <template #icon>
                            <i-tabler-dots />
                          </template>
                        </n-button>
                      </n-dropdown>
                    </n-flex>
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
                    <n-tag v-if="conv.memberCount" size="small" :bordered="false">
                      {{ conv.memberCount }} 人
                    </n-tag>
                  </n-flex>
                </div>
              </div>
            </template>
          </n-spin>
        </n-card>
      </n-gi>

      <n-gi :span="isMobile ? 1 : 17">
        <n-card :bordered="false" class="chat-card">
          <template #header>
            <n-flex align="center" justify="space-between" :wrap="false">
              <n-text strong>
                {{ selectedConversation ? conversationLabel(selectedConversation) : '未选择会话' }}
              </n-text>
              <n-flex align="center" size="small" :wrap="false">
                <n-text v-if="selectedConversation" depth="3" class="chat-key">
                  {{ selectedConversation.key }}
                </n-text>
                <n-button
                  v-if="selectedConversation?.kind === 'group'"
                  size="tiny"
                  secondary
                  :loading="refreshingInfo"
                  @click="refreshGroupInfo"
                >
                  刷新群资料
                </n-button>
              </n-flex>
            </n-flex>
          </template>

          <ListEmptyState v-if="!selectedKey" description="从左侧选择一个会话开始" />

          <template v-else>
            <n-text
              v-if="selectedConversation?.classText || selectedConversation?.tags?.length"
              depth="3"
              class="conv-info"
            >
              <template v-if="selectedConversation?.classText">
                分类：{{ selectedConversation.classText }}
              </template>
              <template v-if="selectedConversation?.tags?.length">
                · 标签：{{ selectedConversation.tags.join('、') }}
              </template>
            </n-text>

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
                <n-flex align="center" size="small" :wrap="false" class="msg-meta">
                  <n-avatar round :size="20" :src="avatarSrc(item.avatarUrl)">
                    {{ chatConsoleAvatarFallback(item.senderName) }}
                  </n-avatar>
                  <n-text depth="3">{{ item.senderName }} · {{ formatChatConsoleTime(item.ts) }}</n-text>
                </n-flex>

                <div class="msg-bubble">
                  <n-text v-if="item.recalled" depth="3" italic>已撤回</n-text>
                  <template v-else>
                    <div v-if="item.quote" class="msg-quote">
                      <n-text depth="3">{{ item.quote.sender }}：{{ item.quote.text }}</n-text>
                    </div>
                    <div v-if="item.card" class="msg-card">
                      <img
                        v-if="item.card.preview"
                        :src="mediaSrc(item.card.preview)"
                        class="msg-card-preview"
                        alt=""
                        loading="lazy"
                      />
                      <n-flex vertical size="small" class="msg-card-body">
                        <n-text strong class="msg-card-title">
                          {{ item.card.title || item.card.arkName || '卡片消息' }}
                        </n-text>
                        <n-text v-if="item.card.desc" depth="3" class="msg-card-desc">
                          {{ item.card.desc }}
                        </n-text>
                        <n-flex align="center" size="small" :wrap="false">
                          <n-tag v-if="item.card.source" size="small" :bordered="false">
                            {{ item.card.source }}
                          </n-tag>
                          <n-text v-if="item.card.arkName" depth="3" class="msg-card-kind">
                            {{ item.card.arkName }}
                          </n-text>
                          <a
                            v-if="item.card.url"
                            :href="item.card.url"
                            target="_blank"
                            rel="noreferrer"
                            class="msg-card-link"
                          >
                            打开
                          </a>
                        </n-flex>
                      </n-flex>
                    </div>
                    <n-text v-if="showMessageText(item)" class="msg-text">{{ item.text }}</n-text>
                    <template v-for="(media, index) in item.media ?? []" :key="index">
                      <n-image
                        v-if="isChatConsoleImage(media.kind)"
                        :src="mediaSrc(media.url)"
                        :width="isMobile ? 160 : 220"
                        class="msg-media"
                      />
                      <audio
                        v-else-if="media.kind === 'record'"
                        :src="mediaSrc(media.url)"
                        controls
                        class="msg-media"
                      />
                      <video
                        v-else-if="media.kind === 'video'"
                        :src="mediaSrc(media.url)"
                        controls
                        class="msg-media"
                      />
                      <a v-else :href="mediaSrc(media.url)" target="_blank" rel="noreferrer">
                        {{ media.name || chatConsoleMediaLabel(media.kind) }}
                      </a>
                    </template>
                  </template>
                </div>

                <n-flex size="small" class="msg-actions" :wrap="false">
                  <n-button size="tiny" quaternary @click="startQuote(item)">引用</n-button>
                  <n-popconfirm
                    v-if="item.dir === 'out' && item.msgId && !item.recalled"
                    @positive-click="recall(item)"
                  >
                    <template #trigger>
                      <n-button size="tiny" quaternary>撤回</n-button>
                    </template>
                    撤回这条消息？
                  </n-popconfirm>
                  <n-dropdown
                    v-if="canMute(item)"
                    trigger="click"
                    :options="muteOptions"
                    @select="seconds => mute(item, Number(seconds))"
                  >
                    <n-button size="tiny" quaternary>禁言</n-button>
                  </n-dropdown>
                </n-flex>
              </div>
            </div>

            <n-flex v-if="quoteTarget" align="center" justify="space-between" class="composer-quote">
              <n-text depth="3">
                正在引用 {{ quoteTarget.senderName }}：{{ quoteTarget.text || '（附件）' }}
              </n-text>
              <n-button size="tiny" quaternary @click="quoteTarget = null">取消</n-button>
            </n-flex>

            <n-flex vertical size="small" class="composer">
              <n-input
                v-model:value="draft"
                type="textarea"
                :autosize="{ minRows: 2, maxRows: 5 }"
                placeholder="输入消息：Enter 发送，Shift+Enter 换行"
                @keydown="onDraftKeydown"
              />
              <n-flex align="center" justify="end" size="small">
                <input
                  ref="fileInput"
                  type="file"
                  multiple
                  accept="image/*,audio/*,video/*,.pdf,.txt,.zip,.log"
                  class="hidden-input"
                  @change="onFilesPicked"
                />
                <n-button size="small" secondary :loading="uploading" @click="pickFiles">
                  附件
                </n-button>
                <n-text depth="3" class="composer-hint">
                  {{ uploading ? '上传中…' : '图片/语音/视频/文件' }}
                </n-text>
                <n-button type="primary" :loading="sending" :disabled="!canSend" @click="send">
                  发送
                </n-button>
              </n-flex>
            </n-flex>
          </template>
        </n-card>
      </n-gi>
    </n-grid>

    <n-modal v-model:show="renameVisible" preset="card" title="重命名会话" class="rename-dialog">
      <n-input v-model:value="renameDraft" placeholder="留空表示恢复自动获取的名字" />
      <template #footer>
        <n-flex justify="end">
          <n-button @click="renameVisible = false">取消</n-button>
          <n-button type="primary" :loading="renaming" @click="submitRename">保存</n-button>
        </n-flex>
      </template>
    </n-modal>
  </main>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue';
import { breakpointsTailwind, useBreakpoints } from '@vueuse/core';
import { useMessage } from 'naive-ui';
import PageHeader from '@/components/shared/PageHeader.vue';
import ListEmptyState from '@/components/shared/ListEmptyState.vue';
import { notifyError } from '@/features/feedback/notifyError';
import {
  fetchChatConsoleConversations,
  fetchChatConsoleMessages,
  markChatConsoleRead,
  muteChatConsoleMember,
  pinChatConsoleConversation,
  recallChatConsoleMessage,
  refreshChatConsoleGroupInfo,
  renameChatConsoleConversation,
  searchChatConsole,
  sendChatConsoleMessage,
  uploadChatConsoleFile,
  type ChatConsoleConversation,
  type ChatConsoleEvent,
  type ChatConsoleMessage,
  type ChatConsoleQuote,
  type ChatConsoleSearchMessage,
} from '@/api/chatConsole';
import {
  conversationLabel,
  formatChatConsoleTime,
  mergeChatConsoleMessages,
  patchChatConsoleConversations,
  sortChatConsoleConversations,
} from '@/features/chatConsole/viewModel';
import {
  chatConsoleAvatarFallback,
  chatConsoleMediaLabel,
  chatConsoleMediaURL,
  isChatConsoleImage,
} from '@/features/chatConsole/media';
import { useChatConsoleStream } from '@/features/chatConsole/useChatConsoleStream';
import { useAuthStore } from '@/features/auth/store';
import { appPinia } from '@/pinia';

const message = useMessage();
const auth = useAuthStore(appPinia);
const breakpoints = useBreakpoints(breakpointsTailwind);
const isMobile = breakpoints.smaller('md');

const MESSAGE_PAGE_SIZE = 50;
const CONVERSATION_POLL_MS = 5000;
const MESSAGE_POLL_MS = 3000;
const SEARCH_DEBOUNCE_MS = 300;

const token = computed(() => auth.token);

const conversations = ref<ChatConsoleConversation[]>([]);
const convLoading = ref(false);
const selectedKey = ref('');
const messages = ref<ChatConsoleMessage[]>([]);
const hasMore = ref(false);
const msgLoading = ref(false);
const loadingMore = ref(false);
const sending = ref(false);
const uploading = ref(false);
const refreshingInfo = ref(false);
const draft = ref('');
const quoteTarget = ref<ChatConsoleMessage | null>(null);
const messagePane = ref<HTMLElement | null>(null);
const fileInput = ref<HTMLInputElement | null>(null);

const searchQuery = ref('');
const searching = ref(false);
const searchResult = ref<{ conversations: ChatConsoleConversation[]; messages: ChatConsoleSearchMessage[] }>({
  conversations: [],
  messages: [],
});

const renameVisible = ref(false);
const renameDraft = ref('');
const renaming = ref(false);
const renameKey = ref('');

let convTimer: number | undefined;
let msgTimer: number | undefined;
let searchTimer: number | undefined;

const convMenuOptions = [
  { label: '重命名', key: 'rename' },
  { label: '刷新群资料', key: 'group-info' },
  { label: '置顶 / 取消置顶', key: 'pin' },
];

const muteOptions = [
  { label: '禁言 10 分钟', key: '600' },
  { label: '禁言 1 小时', key: '3600' },
  { label: '禁言 1 天', key: '86400' },
  { label: '解除禁言', key: '0' },
];

const selectedConversation = computed(
  () => conversations.value.find(item => item.key === selectedKey.value) ?? null
);
const canSend = computed(
  () => !!selectedKey.value && draft.value.trim().length > 0 && !sending.value
);

const stream = useChatConsoleStream({ onEvent: handleStreamEvent });
const streamTag = computed(() => {
  switch (stream.state.value) {
    case 'connected':
      return { type: 'success' as const, text: '实时' };
    case 'connecting':
      return { type: 'warning' as const, text: '连接中' };
    case 'disconnected':
      return { type: 'default' as const, text: '轮询' };
    default:
      return { type: 'default' as const, text: '轮询' };
  }
});

function avatarSrc(raw: string | undefined): string {
  return chatConsoleMediaURL(raw, token.value);
}

function mediaSrc(raw: string | undefined): string {
  return chatConsoleMediaURL(raw, token.value);
}

/**
 * 有结构化卡片时，正文里那份「[卡片消息] …」是后端拼出来的可读副本，
 * 再显示一遍就重复了；只有正文里还有卡片之外的内容时才显示。
 */
function showMessageText(item: ChatConsoleMessage): boolean {
  if (!item.text) return false;
  if (!item.card) return true;
  return !item.text.startsWith('[卡片消息]');
}

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

function upsertConversation(conv: ChatConsoleConversation) {
  const others = conversations.value.filter(item => item.key !== conv.key);
  conversations.value = sortChatConsoleConversations([...others, conv]);
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
    if (key !== selectedKey.value) return;
    messages.value = append
      ? mergeChatConsoleMessages(page.messages, messages.value)
      : mergeChatConsoleMessages([], page.messages);
    hasMore.value = page.hasMore;
    if (!append) scrollToBottom();
  } catch (error) {
    notifyError(error, message, '加载消息失败');
  }
}

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
    // 轮询失败静默，下一轮再试
  }
}

function startMessagePolling() {
  stopMessagePolling();
  msgTimer = window.setInterval(() => {
    // 实时通道连上时不需要再轮询消息
    if (document.hidden || stream.state.value === 'connected') return;
    void refreshLatestMessages();
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
  quoteTarget.value = null;
  messages.value = [];
  hasMore.value = false;
  msgLoading.value = true;
  startMessagePolling();
  try {
    await loadSelectedMessages();
    await markChatConsoleRead(conv.key);
    await refreshConversations();
  } catch {
    // loadSelectedMessages 已提示
  } finally {
    msgLoading.value = false;
  }
}

async function openSearchMessage(item: ChatConsoleSearchMessage) {
  const conv =
    conversations.value.find(c => c.key === item.conversationKey) ??
    ({
      key: item.conversationKey,
      kind: item.conversationKey.startsWith('group:') ? 'group' : 'c2c',
      targetId: '',
      name: item.conversationName || item.conversationKey,
      nameManual: false,
      fullMessage: false,
      pinned: false,
      lastTs: item.ts,
      lastText: item.text,
      unread: 0,
      updatedAt: item.ts,
      online: true,
    } satisfies ChatConsoleConversation);
  searchQuery.value = '';
  await selectConversation(conv);
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
      const quote: ChatConsoleQuote | undefined = quoteTarget.value
      ? {
          sender: quoteTarget.value.senderName,
          text: quoteTarget.value.text,
          // 后端会把用户消息翻译成 REFIDX_…；这里给的是控制台内的消息 ID
          msgId: quoteTarget.value.msgId || quoteTarget.value.msgIdx,
        }
      : undefined;
    const sent = await sendChatConsoleMessage(key, text, quote);
    messages.value = mergeChatConsoleMessages(messages.value, [sent]);
    draft.value = '';
    quoteTarget.value = null;
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

function pickFiles() {
  fileInput.value?.click();
}

async function onFilesPicked(event: Event) {
  const input = event.target as HTMLInputElement | null;
  const files = Array.from(input?.files ?? []);
  if (input) input.value = '';
  const key = selectedKey.value;
  if (!key || !files.length || uploading.value) return;
  uploading.value = true;
  try {
    for (const file of files) {
      const sent = await uploadChatConsoleFile(key, file);
      messages.value = mergeChatConsoleMessages(messages.value, [sent]);
    }
    scrollToBottom();
    void refreshConversations();
  } catch (error) {
    notifyError(error, message, '发送附件失败');
  } finally {
    uploading.value = false;
  }
}

function startQuote(item: ChatConsoleMessage) {
  quoteTarget.value = item;
}

async function recall(item: ChatConsoleMessage) {
  if (!selectedKey.value || !item.msgId) return;
  try {
    const updated = await recallChatConsoleMessage(selectedKey.value, item.msgId);
    if (updated) messages.value = mergeChatConsoleMessages(messages.value, [updated]);
    message.success('已撤回');
    void refreshConversations();
  } catch (error) {
    notifyError(error, message, '撤回失败');
  }
}

function canMute(item: ChatConsoleMessage): boolean {
  return (
    selectedConversation.value?.kind === 'group' &&
    item.dir === 'in' &&
    !!item.senderOpenId &&
    !item.recalled
  );
}

async function mute(item: ChatConsoleMessage, seconds: number) {
  if (!selectedKey.value || !item.senderOpenId) return;
  try {
    await muteChatConsoleMember(selectedKey.value, item.senderOpenId, seconds);
    message.success(seconds > 0 ? `已禁言 ${item.senderName}` : `已解除 ${item.senderName} 的禁言`);
  } catch (error) {
    notifyError(error, message, '禁言操作失败');
  }
}

async function togglePin(conv: ChatConsoleConversation) {
  try {
    const updated = await pinChatConsoleConversation(conv.key, !conv.pinned);
    if (updated) upsertConversation(updated);
    else await refreshConversations();
  } catch (error) {
    notifyError(error, message, '置顶操作失败');
  }
}

function onConvMenu(key: string, conv: ChatConsoleConversation) {
  if (key === 'pin') {
    void togglePin(conv);
    return;
  }
  if (key === 'group-info') {
    void refreshGroupInfoOf(conv);
    return;
  }
  renameKey.value = conv.key;
  renameDraft.value = conv.nameManual ? conv.name : '';
  renameVisible.value = true;
}

async function refreshGroupInfoOf(conv: ChatConsoleConversation) {
  if (conv.kind !== 'group') {
    message.warning('只有群会话能刷新群资料');
    return;
  }
  refreshingInfo.value = true;
  try {
    const updated = await refreshChatConsoleGroupInfo(conv.key);
    if (updated) upsertConversation(updated);
    message.success('群资料已刷新');
  } catch (error) {
    notifyError(error, message, '刷新群资料失败');
  } finally {
    refreshingInfo.value = false;
  }
}

async function refreshGroupInfo() {
  if (!selectedConversation.value) return;
  await refreshGroupInfoOf(selectedConversation.value);
}

async function submitRename() {
  if (!renameKey.value) return;
  renaming.value = true;
  try {
    const updated = await renameChatConsoleConversation(renameKey.value, renameDraft.value.trim());
    if (updated) upsertConversation(updated);
    renameVisible.value = false;
    message.success('已重命名');
  } catch (error) {
    notifyError(error, message, '重命名失败');
  } finally {
    renaming.value = false;
  }
}

let refreshListTimer: number | undefined;
function scheduleRefreshConversations() {
  if (refreshListTimer) return;
  refreshListTimer = window.setTimeout(() => {
    refreshListTimer = undefined;
    void refreshConversations();
  }, 500);
}

function handleStreamEvent(event: ChatConsoleEvent) {
  if (event.type === 'conversation' && event.conversation) {
    upsertConversation(event.conversation);
    return;
  }
  if (event.type === 'settings') {
    scheduleRefreshConversations();
    return;
  }
  const msg = event.message;
  const conv = event.conversation;
  if (!msg) return;
  scheduleRefreshConversations();
  if (!conv || conv.key !== selectedKey.value) return;
  const stickToBottom = isNearBottom();
  const before = messages.value.length;
  messages.value = mergeChatConsoleMessages(messages.value, [msg]);
  if (messages.value.length !== before) {
    if (stickToBottom) scrollToBottom();
    if (msg.dir === 'in') void markChatConsoleRead(conv.key);
  }
}

watch(searchQuery, value => {
  if (searchTimer) window.clearTimeout(searchTimer);
  const query = value.trim();
  if (!query) {
    searchResult.value = { conversations: [], messages: [] };
    searching.value = false;
    return;
  }
  searching.value = true;
  searchTimer = window.setTimeout(async () => {
    try {
      searchResult.value = await searchChatConsole(query, 30);
    } catch (error) {
      notifyError(error, message, '搜索失败');
      searchResult.value = { conversations: [], messages: [] };
    } finally {
      searching.value = false;
    }
  }, SEARCH_DEBOUNCE_MS);
});

onMounted(async () => {
  conversations.value = sortChatConsoleConversations(
    await fetchChatConsoleConversations().catch(() => [])
  );
  stream.start();
  convTimer = window.setInterval(() => {
    if (!document.hidden) void refreshConversations();
  }, CONVERSATION_POLL_MS);
});

onUnmounted(() => {
  if (convTimer) window.clearInterval(convTimer);
  if (searchTimer) window.clearTimeout(searchTimer);
  if (refreshListTimer) window.clearTimeout(refreshListTimer);
  stopMessagePolling();
  stream.stop();
});
</script>

<style scoped>
.conv-card,
.chat-card {
  height: 100%;
}

.conv-search {
  margin-bottom: 0.5rem;
}

.search-hint {
  display: block;
  margin-bottom: 0.35rem;
  font-size: 0.8rem;
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
  max-width: 9rem;
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

.conv-info {
  display: block;
  margin-bottom: 0.35rem;
  font-size: 0.8rem;
}

.load-more {
  margin-bottom: 0.5rem;
}

.msg-scroll {
  max-height: 50vh;
  overflow-y: auto;
  padding: 0 0.25rem;
}

.msg-row {
  display: flex;
  flex-direction: column;
  margin-bottom: 0.7rem;
}

.msg-row.out {
  align-items: flex-end;
}

.msg-row.in {
  align-items: flex-start;
}

.msg-meta {
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

.msg-quote {
  border-left: 3px solid var(--sd-border-color, rgba(128, 128, 128, 0.4));
  padding-left: 0.4rem;
  margin-bottom: 0.25rem;
  font-size: 0.85rem;
}

.msg-media {
  display: block;
  margin-top: 0.3rem;
  max-width: 100%;
}

/* 结构化卡片（ARK）：预览图 + 标题/描述/来源 + 「打开」 */
.msg-card {
  display: flex;
  gap: 0.5rem;
  margin-bottom: 0.3rem;
  padding: 0.4rem;
  border-radius: 0.5rem;
  background: rgba(128, 128, 128, 0.12);
  max-width: min(28rem, 100%);
}

.msg-card-preview {
  width: 4.5rem;
  height: 4.5rem;
  object-fit: cover;
  border-radius: 0.35rem;
  flex: none;
}

.msg-card-body {
  min-width: 0;
}

.msg-card-title {
  word-break: break-word;
}

.msg-card-desc {
  font-size: 0.85rem;
  word-break: break-word;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.msg-card-kind {
  font-size: 0.75rem;
}

.msg-card-link {
  font-size: 0.8rem;
  color: var(--primary-color, #63e2b7);
}

.msg-actions {
  margin-top: 0.15rem;
  opacity: 0;
  transition: opacity 0.15s ease;
}

.msg-row:hover .msg-actions {
  opacity: 1;
}

/* 触屏设备没有 hover，操作按钮必须常显 */
@media (hover: none) {
  .msg-actions {
    opacity: 1;
  }
}

.composer-quote {
  margin-top: 0.4rem;
  padding: 0.35rem 0.5rem;
  border-radius: 0.4rem;
  background: var(--sd-hover-color, rgba(128, 128, 128, 0.1));
}

.composer {
  margin-top: 0.5rem;
}

.composer-hint {
  font-size: 0.8rem;
}

.chat-key {
  max-width: 18rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.hidden-input {
  display: none;
}

.rename-dialog {
  width: min(520px, calc(100vw - 2rem));
}
</style>
