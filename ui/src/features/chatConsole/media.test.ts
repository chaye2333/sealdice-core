import { describe, expect, it } from 'vitest';
import {
  chatConsoleAvatarFallback,
  chatConsoleMediaLabel,
  chatConsoleMediaURL,
  isChatConsoleImage,
} from './media';

describe('聊天控制台媒体', () => {
  it('代理地址带 url 与 token，并做 URL 编码', () => {
    const url = chatConsoleMediaURL('https://multimedia.nt.qq.com.cn/a b.jpg?x=1&y=2', 'tk 1');
    expect(url).toContain('/sd-api/chat-console/media?url=');
    expect(url).toContain(encodeURIComponent('https://multimedia.nt.qq.com.cn/a b.jpg?x=1&y=2'));
    expect(url).toContain(`token=${encodeURIComponent('tk 1')}`);
  });

  it('空地址或没有令牌时不生成地址（避免 403 的碎图）', () => {
    expect(chatConsoleMediaURL(undefined, 'tk')).toBe('');
    expect(chatConsoleMediaURL('   ', 'tk')).toBe('');
    expect(chatConsoleMediaURL('https://x', '')).toBe('');
  });

  it('媒体类型映射', () => {
    expect(isChatConsoleImage('image')).toBe(true);
    expect(isChatConsoleImage('record')).toBe(false);
    expect(chatConsoleMediaLabel('image')).toBe('图片');
    expect(chatConsoleMediaLabel('record')).toBe('语音');
    expect(chatConsoleMediaLabel('video')).toBe('视频');
    expect(chatConsoleMediaLabel('file')).toBe('文件');
    expect(chatConsoleMediaLabel(undefined)).toBe('文件');
  });

  it('头像兜底文字取首字符', () => {
    expect(chatConsoleAvatarFallback('调查员甲')).toBe('调');
    expect(chatConsoleAvatarFallback('   ')).toBe('?');
    expect(chatConsoleAvatarFallback(undefined)).toBe('?');
  });
});
