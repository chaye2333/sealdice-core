import { describe, expect, it } from 'vitest';
import {
  buildOfficialQQMarkdownForImage,
  buildOfficialQQMarkdownImage,
  officialQQMediaAltText,
} from './markdown';

describe('官方 QQ Markdown 图片引用', () => {
  it('带宽高生成文档里的写法', () => {
    expect(
      buildOfficialQQMarkdownImage({
        fileName: 'kline.png',
        url: 'qqbot-media://image/v1/token123',
        width: 800,
        height: 400,
      })
    ).toBe('![kline #800px #400px](qqbot-media://image/v1/token123)');
  });

  it('没有宽高时只写引用', () => {
    expect(
      buildOfficialQQMarkdownImage({
        fileName: 'kline.png',
        url: 'qqbot-media://image/v1/token123',
      })
    ).toBe('![kline](qqbot-media://image/v1/token123)');
  });

  it('宽高是小数时取整，非法值当没有', () => {
    expect(
      buildOfficialQQMarkdownImage({
        fileName: 'a.gif',
        url: 'qqbot-media://image/v1/x',
        width: 800.6,
        height: 400.2,
      })
    ).toBe('![a #801px #400px](qqbot-media://image/v1/x)');
    expect(
      buildOfficialQQMarkdownImage({ fileName: 'a.gif', url: 'qqbot-media://image/v1/x', width: 0, height: 0 })
    ).toBe('![a](qqbot-media://image/v1/x)');
  });

  it('alt 文本去掉扩展名和破坏语法的字符', () => {
    expect(officialQQMediaAltText('kline.png')).toBe('kline');
    expect(officialQQMediaAltText('a[b]' + String.fromCharCode(10) + 'c.png')).toBe('a b c');
    expect(officialQQMediaAltText('')).toBe('图片');
    expect(officialQQMediaAltText('.png')).toBe('图片');
  });

  it('空引用直接报错，便于界面提示', () => {
    expect(() => buildOfficialQQMarkdownImage({ url: '   ' })).toThrow();
  });

  it('从接口图片对象生成时使用后端补记的宽高', () => {
    expect(
      buildOfficialQQMarkdownForImage({
        imageId: 'img1',
        url: 'qqbot-media://image/v1/t',
        fileName: '海报.jpg',
        width: 1080,
        height: 1920,
      })
    ).toBe('![海报 #1080px #1920px](qqbot-media://image/v1/t)');
  });
});
