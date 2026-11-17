import type { OfficialQQMediaImage } from '@/api/officialQQMedia';

/**
 * 官方 QQ Markdown 图片引用：
 * `![说明 #宽px #高px](qqbot-media://image/v1/<token>)`
 *
 * 宽高在文档里是可选的；拿到就带上（平台按这个尺寸渲染），拿不到就只写引用。
 */
export function buildOfficialQQMarkdownImage(input: {
  fileName?: string;
  url: string;
  width?: number;
  height?: number;
}): string {
  const url = (input.url || '').trim();
  if (!url) throw new Error('图片引用为空');
  const alt = officialQQMediaAltText(input.fileName);
  const width = normalizeSize(input.width);
  const height = normalizeSize(input.height);
  const size = width > 0 && height > 0 ? ` #${width}px #${height}px` : '';
  return `![${alt}${size}](${url})`;
}

/** 从会话/接口返回的图片生成 Markdown（宽高缺失时不带尺寸）。 */
export function buildOfficialQQMarkdownForImage(image: OfficialQQMediaImage): string {
  return buildOfficialQQMarkdownImage({
    fileName: image.fileName,
    url: image.url,
    width: image.width,
    height: image.height,
  });
}

/**
 * alt 文本：去掉扩展名，并清掉会破坏 Markdown 的方括号 / 换行。
 * 例：`kline.png` -> `kline`；`a[b].png` -> `a b`。
 */
export function officialQQMediaAltText(fileName: string | undefined): string {
  const base = (fileName || '').trim().replace(/\.[^.]+$/, '');
  const cleaned = base.replace(/[\[\]\r\n]/g, ' ').replace(/\s+/g, ' ').trim();
  return cleaned || '图片';
}

function normalizeSize(value: number | undefined): number {
  if (!value || !Number.isFinite(value) || value <= 0) return 0;
  return Math.round(value);
}
