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

/** QQ 群号 / QQ 号的合法形态：纯数字，5~12 位。openid 之类一律不算。 */
export function chatConsoleQQNumber(raw: string | undefined): string {
  const value = raw?.trim() ?? '';
  return /^\d{5,12}$/.test(value) ? value : '';
}

/**
 * QQ 群头像地址。
 *
 * 官方 QQ 的会话只有 openid，没有群号，所以那边靠后端算好的 `avatarUrl`；
 * OneBot / 其它平台会把群号放进 `uin`，这类会话用 qlogo 的群头像接口补一个，
 * 否则界面上永远只有「名字首字」的占位圆环。
 */
export function chatConsoleQQGroupAvatarURL(uin: string | undefined): string {
  const number = chatConsoleQQNumber(uin);
  return number ? `https://p.qlogo.cn/gh/${number}/${number}/0` : '';
}

/** QQ 用户头像地址（同样需要真实 QQ 号；openid 拼不出头像）。 */
export function chatConsoleQQUserAvatarURL(uin: string | undefined): string {
  const number = chatConsoleQQNumber(uin);
  return number ? `https://q1.qlogo.cn/g?b=qq&nk=${number}&s=140` : '';
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
