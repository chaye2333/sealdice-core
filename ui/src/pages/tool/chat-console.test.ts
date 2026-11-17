// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { nextTick, ref } from 'vue';
import { mount, type VueWrapper } from '@vue/test-utils';
import { appPinia } from '@/pinia';
import { useAuthStore } from '@/features/auth/store';
import ChatConsolePage from './chat-console.vue';

const feedback = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), warning: vi.fn() }));
vi.mock('naive-ui', async original => ({
  ...(await original<typeof import('naive-ui')>()),
  useMessage: () => feedback,
}));

const api = vi.hoisted(() => {
  const conversation = (key: string, name: string) => ({
    key,
    kind: 'group' as const,
    targetId: key,
    name,
    nameManual: false,
    fullMessage: false,
    pinned: false,
    lastTs: 10,
    lastText: '最后一条',
    unread: 1,
    updatedAt: 10,
    online: true,
  });
  return {
    fetchChatConsoleConversations: vi.fn(async () => [
      conversation('group:1', '测试群一'),
      conversation('group:2', '测试群二'),
    ]),
    fetchChatConsoleMessages: vi.fn(async () => ({ messages: [], hasMore: false })),
    fetchChatConsoleStorage: vi.fn(async () => ({
      totalBytes: 0,
      retentionDays: 0,
      conversations: [],
    })),
    markChatConsoleRead: vi.fn(async () => true),
    muteChatConsoleMember: vi.fn(async () => true),
    pinChatConsoleConversation: vi.fn(async () => undefined),
    purgeChatConsole: vi.fn(async () => true),
    recallChatConsoleMessage: vi.fn(async () => undefined),
    refreshChatConsoleGroupInfo: vi.fn(async () => undefined),
    renameChatConsoleConversation: vi.fn(async () => undefined),
    saveChatConsoleRetention: vi.fn(async (days: number) => days),
    searchChatConsole: vi.fn(async () => ({ conversations: [], messages: [] })),
    sendChatConsoleMessage: vi.fn(async () => undefined),
    uploadChatConsoleFile: vi.fn(async () => undefined),
  };
});
vi.mock('@/api/chatConsole', () => api);
vi.mock('@/features/chatConsole/useChatConsoleStream', () => ({
  useChatConsoleStream: () => ({ state: ref('idle'), start: vi.fn(), stop: vi.fn() }),
}));

let wrapper: VueWrapper | undefined;

/** happy-dom 没有真正的媒体查询：按查询方向模拟手机/桌面宽度。 */
function stubViewport(mobile: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => {
    const matches = mobile ? /max-width/.test(query) : /min-width/.test(query);
    return {
      matches,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    };
  });
}

function mountPage() {
  wrapper = mount(ChatConsolePage, {
    attachTo: document.body,
    global: {
      plugins: [appPinia],
      stubs: { PageHeader: true, ListEmptyState: true, ChatConsoleAvatar: true },
    },
  });
  return wrapper;
}

beforeEach(() => {
  useAuthStore(appPinia).setAccessToken('chat-console-test');
  useAuthStore(appPinia).finishInitialization();
});

afterEach(async () => {
  wrapper?.unmount();
  useAuthStore(appPinia).clearAccessToken();
  await nextTick();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

it('手机端先只看会话列表，选中后切到聊天内容，返回按钮回到列表', async () => {
  stubViewport(true);
  mountPage();

  await vi.waitFor(() => expect(wrapper!.findAll('.conv-item')).toHaveLength(2));
  expect(wrapper!.find('.conv-card').exists()).toBe(true);
  expect(wrapper!.find('.chat-card').exists()).toBe(false);

  await wrapper!.findAll('.conv-item')[0]!.trigger('click');
  await vi.waitFor(() => expect(wrapper!.find('.chat-card').exists()).toBe(true));
  expect(wrapper!.find('.conv-card').exists()).toBe(false);
  expect(wrapper!.find('.back-to-list').exists()).toBe(true);

  await wrapper!.find('.back-to-list').trigger('click');
  await vi.waitFor(() => expect(wrapper!.find('.conv-card').exists()).toBe(true));
  expect(wrapper!.find('.chat-card').exists()).toBe(false);
});

it('桌面端两栏并列，不出现返回按钮', async () => {
  stubViewport(false);
  mountPage();

  await vi.waitFor(() => expect(wrapper!.findAll('.conv-item')).toHaveLength(2));
  expect(wrapper!.find('.conv-card').exists()).toBe(true);
  expect(wrapper!.find('.chat-card').exists()).toBe(true);

  await wrapper!.findAll('.conv-item')[0]!.trigger('click');
  await vi.waitFor(() => expect(wrapper!.findAll('.conv-item.active')).toHaveLength(1));
  expect(wrapper!.find('.conv-card').exists()).toBe(true);
  expect(wrapper!.find('.back-to-list').exists()).toBe(false);
});
