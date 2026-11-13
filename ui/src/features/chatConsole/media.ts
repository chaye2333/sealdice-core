import { getApiBaseUrl } from '@/api/config';

/**
 * 官方 QQ 的媒体（头像 / 图片 / 语音）直连会被防盗链或跨域挡掉，
 * 所以走 `/sd-api/chat-console/media` 代理；该接口只放行腾讯的媒体域名。
 *
 * `<img>` 不能带请求头，但 v1 的鉴权同时支持 `?token=`，所以这里把令牌拼进查询串。
 */
export function chatConsoleMediaURL(raw: string | undefined, token: string): string {
  const url = raw?.trim();
  if (!url) return '';
  if (!token) return '';
  const path = `/sd-api/chat-console/media?url=${encodeURIComponent(url)}&token=${encodeURIComponent(
    token
  )}`;
  const base = getApiBaseUrl();
  return base ? `${base}${path}` : path;
}

/** 头像也是同样的代理（QQ 头像来自 thirdqq.qlogo.cn）。 */
export function chatConsoleAvatarURL(raw: string | undefined, token: string): string {
  return chatConsoleMediaURL(raw, token);
}

/**
 * 卡片链接只允许 http/https：`jump_url` 的内容由群里的对方决定，
 * 直接当 `<a href>` 渲染可能被塞进 `javascript:` 之类的伪协议。
 * 后端入库时已经拦了一道，这里对历史数据再兜一次底。
 */
export function isChatConsoleSafeLink(raw: string | undefined): boolean {
  const url = raw?.trim();
  if (!url) return false;
  return /^https?:\/\//i.test(url);
}

export function isChatConsoleImage(kind: string | undefined): boolean {
  return kind === 'image';
}

export function chatConsoleMediaLabel(kind: string | undefined): string {
  switch (kind) {
    case 'image':
      return '图片';
    case 'record':
      return '语音';
    case 'video':
      return '视频';
    default:
      return '文件';
  }
}

/** 头像兜底文字：取名字首字符，避免空头像。 */
export function chatConsoleAvatarFallback(name: string | undefined): string {
  const trimmed = name?.trim();
  if (!trimmed) return '?';
  return Array.from(trimmed)[0] ?? '?';
}
