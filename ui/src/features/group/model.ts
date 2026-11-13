export type GroupQuitTarget = {
  groupId: string;
  groupName?: string;
  diceId?: string;
};

export function formatGroupQuitTarget(target: GroupQuitTarget): string {
  const groupName = target.groupName?.trim() ? '「' + target.groupName.trim() + '」' : '';
  const diceId = target.diceId ? '，账号 ' + target.diceId : '';
  return target.groupId + groupName + diceId;
}

export const defaultGroupQuitText = '因长期不使用等原因，骰主后台操作退出';

const groupQuitDefaultTextStorageKey = 'group-quit-default-text';
type GroupQuitTextStorage = Pick<Storage, 'getItem' | 'setItem'>;

function getBrowserStorage(): GroupQuitTextStorage | undefined {
  if (typeof localStorage === 'undefined') return undefined;
  return localStorage;
}

export function readGroupQuitDefaultText(
  storage: GroupQuitTextStorage | undefined = getBrowserStorage()
): string {
  try {
    return storage?.getItem(groupQuitDefaultTextStorageKey) || defaultGroupQuitText;
  } catch {
    return defaultGroupQuitText;
  }
}

export function writeGroupQuitDefaultText(
  value: string,
  storage: GroupQuitTextStorage | undefined = getBrowserStorage()
): void {
  try {
    storage?.setItem(groupQuitDefaultTextStorageKey, value);
  } catch {
    // 本地存储不可用不影响本次操作。
  }
}

/** 黑白名单的 rank：-30 禁止（拉黑）、-10 警告、30 信任。 */
export const BAN_RANK_BANNED = -30;
export const BAN_RANK_WARNED = -10;
export const BAN_RANK_TRUSTED = 30;

/** 把 rank 翻成界面文案；未登记的等级返回空串（前端只展示这三个档位）。 */
export function groupBanRankLabel(rank: number | undefined): string {
  switch (rank) {
    case BAN_RANK_TRUSTED:
      return '信任';
    case BAN_RANK_BANNED:
      return '禁止';
    case BAN_RANK_WARNED:
      return '警告';
    default:
      return '';
  }
}

/** 该 rank 是否属于「已拉黑」（机器人不再响应这个群）。 */
export function isGroupBannedRank(rank: number | undefined): boolean {
  return rank === BAN_RANK_BANNED;
}

/** 从黑白名单条目里建 id → rank 的索引，只保留群 ID 形态的条目。 */
export function buildGroupBanRankIndex(
  items: ReadonlyArray<{ ID?: string; rank?: number }> | null | undefined
): Map<string, number> {
  const index = new Map<string, number>();
  for (const item of items ?? []) {
    const id = item?.ID?.trim();
    if (!id || !id.includes('Group')) continue;
    if (typeof item.rank !== 'number') continue;
    index.set(id, item.rank);
  }
  return index;
}

