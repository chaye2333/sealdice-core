import { describe, expect, it } from 'vitest';
import {
  chatConsoleAvatarFallback,
  chatConsoleMediaLabel,
  chatConsoleMediaURL,
  chatConsoleQQGroupAvatarURL,
  chatConsoleQQNumber,
  chatConsoleQQUserAvatarURL,
  isChatConsoleImage,
  isChatConsoleSafeLink,
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

  it('卡片链接只放行 http/https（挡住 javascript: 之类的伪协议）', () => {
    expect(isChatConsoleSafeLink('https://example.com/a')).toBe(true);
    expect(isChatConsoleSafeLink('HTTP://example.com/a')).toBe(true);
    expect(isChatConsoleSafeLink('javascript:alert(1)')).toBe(false);
    expect(isChatConsoleSafeLink(' JavaScript:alert(1)')).toBe(false);
    expect(isChatConsoleSafeLink('data:text/html,<script>alert(1)</script>')).toBe(false);
    expect(isChatConsoleSafeLink('//evil.example.com/a')).toBe(false);
    expect(isChatConsoleSafeLink('')).toBe(false);
    expect(isChatConsoleSafeLink(undefined)).toBe(false);
  });

  it('只有真实 QQ 号才拼头像，openid 之类一律放弃', () => {
    expect(chatConsoleQQNumber('123456789')).toBe('123456789');
    expect(chatConsoleQQNumber(' 12345 ')).toBe('12345');
    expect(chatConsoleQQNumber('ABCDEF0123456789')).toBe('');
    expect(chatConsoleQQNumber('123')).toBe('');
    expect(chatConsoleQQNumber(undefined)).toBe('');
  });

  it('群头像 / 用户头像地址来自 qlogo（媒体代理放行 .qlogo.cn）', () => {
    expect(chatConsoleQQGroupAvatarURL('123456789')).toBe(
      'https://p.qlogo.cn/gh/123456789/123456789/0'
    );
    expect(chatConsoleQQUserAvatarURL('123456789')).toBe(
      'https://q1.qlogo.cn/g?b=qq&nk=123456789&s=140'
    );
    expect(chatConsoleQQGroupAvatarURL('openid-abc')).toBe('');
    expect(chatConsoleQQUserAvatarURL(undefined)).toBe('');
  });
});