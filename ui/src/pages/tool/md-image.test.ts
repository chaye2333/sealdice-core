// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { nextTick } from 'vue';
import { mount, type VueWrapper } from '@vue/test-utils';
import { appPinia } from '@/pinia';
import { useAuthStore } from '@/features/auth/store';
import MdImagePage from './md-image.vue';

const feedback = vi.hoisted(() => ({
  success: vi.fn(),
  error: vi.fn(),
  warning: vi.fn(),
  info: vi.fn(),
}));
vi.mock('naive-ui', async original => ({
  ...(await original<typeof import('naive-ui')>()),
  useMessage: () => feedback,
}));
const clipboard = vi.hoisted(() => ({ copyText: vi.fn(async () => {}) }));
vi.mock('@/features/clipboard/copyText', () => clipboard);
vi.mock('@/features/testMode/state', async () => {
  const { ref } = await import('vue');
  return { useTestMode: () => ({ isTestMode: ref(false) }) };
});

const api = vi.hoisted(() => {
  const make = (
    imageId: string,
    fileName: string,
    extra: { width?: number; height?: number; previewUrl?: string } = {}
  ) => ({
    imageId,
    url: `qqbot-media://image/v1/${imageId}`,
    fileName,
    createdAt: 1792051409,
    expiresAt: 1792656210,
    ...extra,
  });
  const images = [
    make('img1', 'kline.png', {
      width: 800,
      height: 400,
      previewUrl: 'https://example.myqcloud.com/a.png',
    }),
    make('img2', 'banner.jpg'),
  ];
  return {
    fetchOfficialQQMediaImages: vi.fn(async () => ({
      bots: [{ id: 'ep1', name: '素材机器人', appId: 'app1' }],
      endpointId: 'ep1',
      images: images.map(image => ({ ...image })),
      total: 2,
      nextCursor: '',
    })),
    uploadOfficialQQMediaImage: vi.fn(async () => ({
      image: make('img3', 'new.png', { width: 100, height: 200 }),
      endpointId: 'ep1',
    })),
    deleteOfficialQQMediaImage: vi.fn(async () => {}),
  };
});
vi.mock('@/api/officialQQMedia', () => api);

let wrapper: VueWrapper | undefined;

function mountPage() {
  wrapper = mount(MdImagePage, {
    attachTo: document.body,
    global: {
      plugins: [appPinia],
      stubs: { PageHeader: true },
    },
  });
  return wrapper;
}

function findButton(text: string) {
  return wrapper!.findAll('button').find(button => button.text().trim() === text);
}

beforeEach(() => {
  useAuthStore(appPinia).setAccessToken('md-image-test');
  useAuthStore(appPinia).finishInitialization();
});

afterEach(async () => {
  wrapper?.unmount();
  useAuthStore(appPinia).clearAccessToken();
  await nextTick();
  vi.clearAllMocks();
});

it('列出素材并通过「复制 Markdown」自动带宽高', async () => {
  mountPage();
  await vi.waitFor(() => expect(wrapper!.findAll('.md-image-item')).toHaveLength(2));

  const text = wrapper!.text();
  expect(text).toContain('2/100');
  expect(text).toContain('800 × 400');
  expect(text).toContain('未知');

  const copyButtons = wrapper!.findAll('button').filter(b => b.text().trim() === '复制 Markdown');
  expect(copyButtons).toHaveLength(2);

  await copyButtons[0]!.trigger('click');
  await vi.waitFor(() =>
    expect(clipboard.copyText).toHaveBeenCalledWith(
      '![kline #800px #400px](qqbot-media://image/v1/img1)'
    )
  );

  await copyButtons[1]!.trigger('click');
  await vi.waitFor(() =>
    expect(clipboard.copyText).toHaveBeenCalledWith('![banner](qqbot-media://image/v1/img2)')
  );
});

it('没拿到登录 token 时不请求，解锁后自动加载', async () => {
  useAuthStore(appPinia).clearAccessToken();
  mountPage();
  await vi.waitFor(() => expect(wrapper!.text()).toContain('还没有上传图片'));
  expect(api.fetchOfficialQQMediaImages).not.toHaveBeenCalled();

  useAuthStore(appPinia).setAccessToken('md-image-test');
  await vi.waitFor(() => expect(api.fetchOfficialQQMediaImages).toHaveBeenCalled());
});

it('没有图片时给出空状态', async () => {
  api.fetchOfficialQQMediaImages.mockResolvedValueOnce({
    bots: [],
    endpointId: '',
    images: [],
    total: 0,
    nextCursor: '',
  });
  mountPage();
  await vi.waitFor(() => expect(api.fetchOfficialQQMediaImages).toHaveBeenCalled());
  await vi.waitFor(() => expect(wrapper!.text()).toContain('还没有上传图片'));
});
