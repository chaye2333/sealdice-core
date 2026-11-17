<template>
  <main class="md-image-page sd-page-flow">
    <PageHeader title="MD 图片管理" />

    <TipBox v-if="errorText" type="error">{{ errorText }}</TipBox>
    <TipBox v-else-if="loaded && !bots.length" type="warning">
      还没有接入官方 QQ 机器人。先到「账号连接」里添加并连上官方 QQ 机器人，才能管理 Markdown 图片素材。
    </TipBox>

    <n-card :bordered="false">
      <n-flex vertical size="small">
        <n-text>
          官方 QQ 给每个机器人 100 张 Markdown 图片额度。上传后得到
          <n-text code>qqbot-media://</n-text> 引用，在 Markdown 消息里写
          <n-text code>![说明 #宽px #高px](qqbot-media://image/v1/…)</n-text>
          即可，引用不依赖公网地址。
        </n-text>
        <n-text depth="3">
          限制：单张 ≤ 10MB，仅 png / jpeg / gif / webp；图片在 90 天内被消息引用会自动续期；
          删除图片可以释放额度。本页上传时会自动补上图片宽高。
        </n-text>
      </n-flex>
    </n-card>

    <n-card :bordered="false">
      <template #header>
        <n-flex align="center" justify="space-between" :wrap="false">
          <n-flex align="center" size="small" :wrap="false">
            <n-text strong>图片素材</n-text>
            <n-tag size="small" :bordered="false" :type="total >= limit ? 'error' : 'default'">
              {{ total }}/{{ limit }}
            </n-tag>
            <n-select
              v-if="bots.length > 1"
              size="small"
              class="md-image-bot"
              :value="endpointId || undefined"
              :options="botOptions"
              placeholder="选择机器人"
              @update:value="(value: string) => onBotChange(value)"
            />
          </n-flex>
          <n-flex align="center" size="small" :wrap="false">
            <input
              ref="fileInput"
              type="file"
              accept="image/png,image/jpeg,image/gif,image/webp"
              class="hidden-input"
              @change="onFilesPicked"
            />
            <n-button size="small" secondary :loading="loading" @click="reload()">刷新</n-button>
            <n-tooltip :disabled="!uploadDisabledReason">
              <template #trigger>
                <n-button
                  size="small"
                  type="primary"
                  :loading="uploading"
                  :disabled="!!uploadDisabledReason"
                  @click="pickFiles"
                >
                  上传图片
                </n-button>
              </template>
              {{ uploadDisabledReason }}
            </n-tooltip>
          </n-flex>
        </n-flex>
      </template>

      <ListEmptyState
        v-if="!images.length && !loading"
        description="还没有上传图片；点右上角「上传图片」开始"
      />

      <n-spin :show="loading && !images.length">
        <div v-if="images.length" class="md-image-grid">
          <n-card
            v-for="item in images"
            :key="item.imageId"
            size="small"
            :bordered="false"
            class="md-image-item"
          >
            <div class="md-image-thumb">
              <img
                v-if="previewSrc(item)"
                :src="previewSrc(item)"
                :alt="item.fileName"
                loading="lazy"
                @load="onThumbLoad(item, $event)"
              />
              <i-tabler-photo v-else class="md-image-thumb-fallback" />
            </div>
            <n-flex vertical size="small">
              <n-text strong class="md-image-name" :title="item.fileName">
                {{ item.fileName || '未命名图片' }}
              </n-text>
              <n-text depth="3" class="md-image-meta">尺寸 {{ sizeText(item) }}</n-text>
              <n-text depth="3" class="md-image-meta">
                上传 {{ formatTs(item.createdAt) }}
              </n-text>
              <n-text depth="3" class="md-image-meta">
                有效期至 {{ formatTs(item.expiresAt) }}
              </n-text>
              <n-flex size="small" :wrap="false">
                <n-button size="tiny" secondary @click="copyMarkdown(item)">复制 Markdown</n-button>
                <n-popconfirm @positive-click="remove(item)">
                  <template #trigger>
                    <n-button size="tiny" quaternary type="error" :disabled="isTestMode">
                      删除
                    </n-button>
                  </template>
                  删除后引用链接立即失效，确定删除这张图片？
                </n-popconfirm>
              </n-flex>
            </n-flex>
          </n-card>
        </div>
      </n-spin>

      <n-flex v-if="nextCursor" justify="center" class="md-image-more">
        <n-button size="small" secondary :loading="loadingMore" @click="loadMore">
          加载更多
        </n-button>
      </n-flex>
    </n-card>
  </main>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue';
import dayjs from 'dayjs';
import { useMessage } from 'naive-ui';
import PageHeader from '@/components/shared/PageHeader.vue';
import ListEmptyState from '@/components/shared/ListEmptyState.vue';
import TipBox from '@/components/shared/TipBox.vue';
import { useAuthStore } from '@/features/auth/store';
import { appPinia } from '@/pinia';
import { copyText } from '@/features/clipboard/copyText';
import { notifyError } from '@/features/feedback/notifyError';
import { chatConsoleMediaURL } from '@/features/chatConsole/media';
import { useTestMode } from '@/features/testMode/state';
import {
  deleteOfficialQQMediaImage,
  fetchOfficialQQMediaImages,
  uploadOfficialQQMediaImage,
  type OfficialQQMediaBot,
  type OfficialQQMediaImage,
} from '@/api/officialQQMedia';
import { buildOfficialQQMarkdownImage } from '@/features/officialQQMedia/markdown';

const message = useMessage();
const { isTestMode } = useTestMode();
const auth = useAuthStore(appPinia);
const token = computed(() => auth.token);

const IMAGE_LIMIT = 100;
const MAX_BYTES = 10 << 20;

const bots = ref<OfficialQQMediaBot[]>([]);
const endpointId = ref('');
const images = ref<OfficialQQMediaImage[]>([]);
const total = ref(0);
const nextCursor = ref('');
const loading = ref(false);
const loadingMore = ref(false);
const uploading = ref(false);
const loaded = ref(false);
const errorText = ref('');
const fileInput = ref<HTMLInputElement | null>(null);
/** 平台不返回宽高时，用缩略图加载后的 naturalWidth/Height 兜底。 */
const measured = reactive<Record<string, { width: number; height: number }>>({});

const limit = IMAGE_LIMIT;

const botOptions = computed(() =>
  bots.value.map(bot => ({ label: `${bot.name}（${bot.appId}）`, value: bot.id }))
);

const uploadDisabledReason = computed(() => {
  if (isTestMode.value) return '展示模式不支持上传';
  if (loaded.value && !bots.value.length) return '还没有接入官方 QQ 机器人';
  if (total.value >= IMAGE_LIMIT) return `已达到 ${IMAGE_LIMIT} 张上限，请先删除不用的图片`;
  return '';
});

function formatTs(ts: number | undefined): string {
  return ts ? dayjs.unix(ts).format('YYYY-MM-DD HH:mm') : '—';
}

function previewSrc(item: OfficialQQMediaImage): string {
  if (!item.previewUrl) return '';
  return chatConsoleMediaURL(item.previewUrl, token.value);
}

function effectiveSize(item: OfficialQQMediaImage): { width: number; height: number } {
  if (item.width && item.height) return { width: item.width, height: item.height };
  return measured[item.imageId] ?? { width: 0, height: 0 };
}

function sizeText(item: OfficialQQMediaImage): string {
  const size = effectiveSize(item);
  return size.width > 0 && size.height > 0 ? `${size.width} × ${size.height}` : '未知';
}

/** 缩略图加载后记下真实宽高；没有后端宽高的老图片也能复制出带尺寸的引用。 */
function onThumbLoad(item: OfficialQQMediaImage, event: Event) {
  if (item.width && item.height) return;
  const el = event.target as HTMLImageElement | null;
  if (!el || !el.naturalWidth || !el.naturalHeight) return;
  measured[item.imageId] = { width: el.naturalWidth, height: el.naturalHeight };
}

async function reload(reset = true) {
  loading.value = true;
  if (reset) errorText.value = '';
  try {
    const list = await fetchOfficialQQMediaImages({ endpointId: endpointId.value });
    bots.value = list.bots;
    if (reset) {
      images.value = list.images;
      nextCursor.value = list.nextCursor;
      total.value = list.total;
      if (!endpointId.value && list.endpointId) endpointId.value = list.endpointId;
    } else {
      const seen = new Set(images.value.map(item => item.imageId));
      images.value = [...images.value, ...list.images.filter(item => !seen.has(item.imageId))];
      nextCursor.value = list.nextCursor;
      total.value = list.total;
    }
  } catch (error) {
    errorText.value = error instanceof Error ? error.message : '加载图片列表失败';
    if (!images.value.length) notifyError(error, message, '加载图片列表失败');
  } finally {
    loading.value = false;
    loaded.value = true;
  }
}

async function loadMore() {
  if (!nextCursor.value || loadingMore.value) return;
  loadingMore.value = true;
  try {
    const list = await fetchOfficialQQMediaImages({
      endpointId: endpointId.value,
      cursor: nextCursor.value,
    });
    const seen = new Set(images.value.map(item => item.imageId));
    images.value = [...images.value, ...list.images.filter(item => !seen.has(item.imageId))];
    nextCursor.value = list.nextCursor;
    total.value = list.total;
  } catch (error) {
    notifyError(error, message, '加载更多图片失败');
  } finally {
    loadingMore.value = false;
  }
}

function onBotChange(value: string) {
  if (value === endpointId.value) return;
  endpointId.value = value;
  nextCursor.value = '';
  void reload();
}

function pickFiles() {
  fileInput.value?.click();
}

async function onFilesPicked(event: Event) {
  const input = event.target as HTMLInputElement | null;
  const file = input?.files?.[0];
  if (input) input.value = '';
  if (!file) return;
  if (file.size > MAX_BYTES) {
    message.error('图片超过 10MB 上限');
    return;
  }
  if (!/^image\/(png|jpeg|gif|webp)$/.test(file.type)) {
    message.error('只支持 png / jpeg / gif / webp 图片');
    return;
  }
  if (uploading.value) return;
  uploading.value = true;
  try {
    const result = await uploadOfficialQQMediaImage(endpointId.value, file);
    endpointId.value = result.endpointId || endpointId.value;
    images.value = [result.image, ...images.value.filter(item => item.imageId !== result.image.imageId)];
    if (result.image.width && result.image.height) {
      measured[result.image.imageId] = {
        width: result.image.width,
        height: result.image.height,
      };
    }
    total.value += 1;
    message.success('上传成功，已自动记住图片宽高');
  } catch (error) {
    notifyError(error, message, '上传图片失败');
  } finally {
    uploading.value = false;
  }
}

async function copyMarkdown(item: OfficialQQMediaImage) {
  const size = effectiveSize(item);
  const snippet = buildOfficialQQMarkdownImage({
    fileName: item.fileName,
    url: item.url,
    width: size.width,
    height: size.height,
  });
  try {
    await copyText(snippet);
    if (size.width > 0 && size.height > 0) {
      message.success('已复制 Markdown 片段');
    } else {
      message.warning('没拿到图片宽高，已复制不带尺寸的引用（可刷新后重试）');
    }
  } catch (error) {
    notifyError(error, message, '复制失败');
  }
}

async function remove(item: OfficialQQMediaImage) {
  if (isTestMode.value) return;
  try {
    await deleteOfficialQQMediaImage(endpointId.value, item.imageId);
    images.value = images.value.filter(image => image.imageId !== item.imageId);
    total.value = Math.max(0, total.value - 1);
    message.success('已删除');
  } catch (error) {
    notifyError(error, message, '删除图片失败');
  }
}

onMounted(() => {
  void reload();
});
</script>

<style scoped>
.hidden-input {
  display: none;
}

.md-image-bot {
  width: 15rem;
}

.md-image-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 0.75rem;
}

.md-image-item {
  height: 100%;
}

.md-image-thumb {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 120px;
  margin-bottom: 0.35rem;
  border-radius: 0.4rem;
  overflow: hidden;
  background: var(--sd-hover-color, rgba(128, 128, 128, 0.1));
}

.md-image-thumb img {
  width: 100%;
  height: 100%;
  object-fit: contain;
}

.md-image-thumb-fallback {
  font-size: 2rem;
  opacity: 0.4;
}

.md-image-name {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.md-image-meta {
  font-size: 0.78rem;
}

.md-image-more {
  margin-top: 0.75rem;
}

@media (max-width: 768px) {
  .md-image-grid {
    grid-template-columns: repeat(auto-fill, minmax(140px, 1fr));
  }

  .md-image-thumb {
    height: 96px;
  }

  .md-image-bot {
    width: 8.5rem;
  }
}
</style>
