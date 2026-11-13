<template>
  <!--
    ⚠️ Naive UI 的 n-avatar 只要 default 插槽有内容，就只渲染文字、**完全不管 src**
    （见 naive-ui/es/avatar/src/Avatar.mjs 的 render：`if (children) return span(text)`）。
    所以「有图显示图、没图显示首字」必须用两个分支，不能在同一个组件里塞插槽兜底，
    否则头像永远显示成首字 —— 聊天控制台之前就是这么「头像加载不出来」的。
  -->
  <n-avatar v-if="src" round :size="size" :src="src" />
  <n-avatar v-else round :size="size" :color="color">
    {{ chatConsoleAvatarFallback(name) }}
  </n-avatar>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { chatConsoleAvatarColor, chatConsoleAvatarFallback } from '@/features/chatConsole/media';

const props = withDefaults(
  defineProps<{
    /** 走媒体代理后的图片地址；空字符串表示没有可用头像 */
    src?: string;
    /** 用来取首字与兜底配色的名字 */
    name?: string;
    size?: number;
  }>(),
  { src: '', name: '', size: 24 }
);

const color = computed(() => chatConsoleAvatarColor(props.name));
</script>
