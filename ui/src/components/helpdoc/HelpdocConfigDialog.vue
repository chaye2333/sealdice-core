<template>
  <n-modal
    v-model:show="show"
    preset="card"
    title="设置帮助文档"
    class="the-dialog"
    :mask-closable="false"
  >
    <h4 class="config-title">分组别名</h4>
    <n-form label-placement="left">
      <n-form-item v-for="group in groups" :key="group.value" :label="group.label">
        <HelpConfigTags
          :group="group"
          :aliases="aliases"
          @add-alias="(groupKey, alias) => emit('addAlias', groupKey, alias)"
          @remove-alias="(groupKey, alias) => emit('removeAlias', groupKey, alias)"
        />
      </n-form-item>
    </n-form>

    <h4 class="config-title">图片化渲染</h4>
    <n-form label-placement="left" label-width="auto">
      <n-form-item>
        <template #label>启用</template>
        <n-checkbox v-model:checked="imageRender.imageRenderEnable">
          帮助文档正文命中敏感词或过长时改发图片
        </n-checkbox>
      </n-form-item>
      <n-form-item label="渲染后端地址">
        <n-input
          v-model:value="imageRender.imageRenderUrl"
          :disabled="!imageRender.imageRenderEnable"
          placeholder="http://127.0.0.1:3212/render"
        />
      </n-form-item>
      <n-form-item label="鉴权令牌">
        <n-input
          v-model:value="imageRender.imageRenderToken"
          :disabled="!imageRender.imageRenderEnable"
          placeholder="可选，按 Authorization: Bearer 发送"
        />
      </n-form-item>
      <n-form-item label="转图长度阈值">
        <n-input-number
          v-model:value="imageRender.imageRenderMinLength"
          :min="0"
          :disabled="!imageRender.imageRenderEnable"
          class="config-number"
        />
        <n-text depth="3" class="config-hint">字符数；0 表示只按敏感词触发</n-text>
      </n-form-item>
      <n-form-item label="单次超时(秒)">
        <n-input-number
          v-model:value="imageRender.imageRenderTimeoutSec"
          :min="0"
          :max="120"
          :disabled="!imageRender.imageRenderEnable"
          class="config-number"
        />
        <n-text depth="3" class="config-hint">0 表示使用默认 15 秒</n-text>
      </n-form-item>
      <n-form-item label="出图宽度">
        <n-input-number
          v-model:value="imageRender.imageRenderWidth"
          :min="0"
          :max="4096"
          :disabled="!imageRender.imageRenderEnable"
          class="config-number"
        />
        <n-text depth="3" class="config-hint">像素；0 表示使用默认 820</n-text>
      </n-form-item>
      <n-form-item>
        <template #label>图片内打码</template>
        <n-checkbox
          v-model:checked="imageRender.imageRenderMaskInImage"
          :disabled="!imageRender.imageRenderEnable"
        >
          图片里也把敏感词打码（更保守，但内容会缺一块）
        </n-checkbox>
      </n-form-item>
      <n-form-item>
        <n-text depth="3">
          需要骰主自部署渲染后端；渲染失败会一律回退成文本，不会让帮助功能整条挂掉。
        </n-text>
      </n-form-item>
    </n-form>

    <template #footer>
      <n-flex justify="end">
        <n-button @click="show = false">取消</n-button>
        <n-button type="primary" :loading="saving" @click="emit('save')"> 保存 </n-button>
      </n-flex>
    </template>
  </n-modal>
</template>

<script setup lang="ts">
import HelpConfigTags from './HelpConfigTags.vue';
import type { HelpdocImageRenderConfig } from '@/features/helpdoc/viewModel';

const show = defineModel<boolean>('show', { required: true });
const imageRender = defineModel<HelpdocImageRenderConfig>('imageRender', { required: true });

defineProps<{
  groups: { label: string; value: string }[];
  aliases: Map<string, string[]>;
  saving: boolean;
}>();

const emit = defineEmits<{
  save: [];
  addAlias: [group: string, alias: string];
  removeAlias: [group: string, alias: string];
}>();
</script>

<style scoped>
.config-title {
  margin: 1rem 0 0.75rem;
}

.config-number {
  width: 12rem;
}

.config-hint {
  margin-left: 0.75rem;
  font-size: 0.85rem;
}
</style>
