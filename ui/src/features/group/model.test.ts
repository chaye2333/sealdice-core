import { describe, expect, it } from 'vitest';
import {
  BAN_RANK_BANNED,
  buildGroupBanRankIndex,
  defaultGroupQuitText,
  formatGroupQuitTarget,
  groupBanRankLabel,
  isGroupBannedRank,
  readGroupQuitDefaultText,
  writeGroupQuitDefaultText,
} from './model';

describe('退群模型', () => {
  it('格式化退群目标的名称和账号', () => {
    expect(formatGroupQuitTarget({ groupId: 'group-1', groupName: '测试群', diceId: 'QQ:1' })).toBe(
      'group-1「测试群」，账号 QQ:1'
    );
    expect(formatGroupQuitTarget({ groupId: 'group-2', groupName: '  ' })).toBe('group-2');
  });

  it('持久化默认退群理由并在存储不可用时降级', () => {
    const values = new Map<string, string>();
    const storage = {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => {
        values.set(key, value);
      },
    };
    expect(readGroupQuitDefaultText(storage)).toBe(defaultGroupQuitText);
    writeGroupQuitDefaultText('测试退群文案', storage);
    expect(readGroupQuitDefaultText(storage)).toBe('测试退群文案');
    const broken = {
      getItem(): string | null {
        throw new Error('unavailable');
      },
      setItem(): void {
        throw new Error('unavailable');
      },
    };
    expect(readGroupQuitDefaultText(broken)).toBe(defaultGroupQuitText);
    expect(() => writeGroupQuitDefaultText('不抛出', broken)).not.toThrow();
  });
});

describe('群黑白名单', () => {
  it('只有 rank=-30 视为已拉黑，并给出档位文案', () => {
    expect(isGroupBannedRank(BAN_RANK_BANNED)).toBe(true);
    expect(isGroupBannedRank(-10)).toBe(false);
    expect(isGroupBannedRank(30)).toBe(false);
    expect(isGroupBannedRank(undefined)).toBe(false);

    expect(groupBanRankLabel(BAN_RANK_BANNED)).toBe('禁止');
    expect(groupBanRankLabel(-10)).toBe('警告');
    expect(groupBanRankLabel(30)).toBe('信任');
    expect(groupBanRankLabel(0)).toBe('');
  });

  it('建索引时只保留群 ID 形态的条目', () => {
    const index = buildGroupBanRankIndex([
      { ID: 'QQ-Group:1', rank: -30 },
      { ID: 'OpenQQ-Group:2-3', rank: 30 },
      { ID: 'QQ:100', rank: -30 },
      { ID: 'QQ-Group:4' },
      { ID: '   ', rank: -30 },
    ]);
    expect([...index.keys()]).toEqual(['QQ-Group:1', 'OpenQQ-Group:2-3']);
    expect(index.get('QQ-Group:1')).toBe(BAN_RANK_BANNED);
    expect(buildGroupBanRankIndex(null).size).toBe(0);
  });
});
