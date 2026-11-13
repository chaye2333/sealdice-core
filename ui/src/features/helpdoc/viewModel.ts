import { trim } from 'es-toolkit/compat';
import type { HelpDoc, HelpTextVo } from '@/api';

export type HelpdocItemQueryModel = {
  pageNum: number;
  pageSize: number;
  id: number | null;
  group: string | null;
  from: string;
  title: string;
};

export function createDefaultHelpdocItemQuery(): HelpdocItemQueryModel {
  return {
    pageNum: 1,
    pageSize: 20,
    id: null,
    group: null,
    from: '',
    title: '',
  };
}

export type GroupOption = {
  label: string;
  value: string;
};

export type HelpDocTreeOption = {
  key: string;
  label: string;
  raw: HelpDoc;
  disabled?: boolean;
  children?: HelpDocTreeOption[];
  icon: 'folder' | 'json' | 'xlsx' | 'file';
  tag?: ReturnType<typeof getHelpdocTag>;
};

export function normalizeHelpdocAliases(aliases: Record<string, string[] | null | undefined>) {
  const result = new Map<string, string[]>();
  for (const [key, value] of Object.entries(aliases)) {
    result.set(key, Array.isArray(value) ? [...value] : []);
  }
  return result;
}

export function buildHelpdocGroupOptions(tree: HelpDoc[] | undefined) {
  const groups: GroupOption[] = [{ label: '默认', value: 'default' }];
  for (const entry of tree ?? []) {
    if (entry.isDir && entry.name) {
      groups.push({ label: entry.name, value: entry.name });
    }
  }
  return groups;
}

export function getHelpdocTag(loadStatus: number, deleted: boolean, group: string) {
  if (loadStatus === 0) return { type: 'warning' as const, label: '未加载' };
  if (loadStatus === 2) return { type: 'error' as const, label: '格式有误' };
  if (deleted) return { type: 'warning' as const, label: group || 'default' };
  return { type: 'success' as const, label: group || 'default' };
}

export function convertHelpdocTree(doc: HelpDoc): HelpDocTreeOption {
  return {
    key: doc.key,
    label: doc.name,
    raw: doc,
    disabled: doc.deleted,
    children: doc.children?.map(convertHelpdocTree) ?? undefined,
    icon: doc.isDir
      ? 'folder'
      : doc.type === '.json'
        ? 'json'
        : doc.type === '.xlsx'
          ? 'xlsx'
          : 'file',
    tag: doc.isDir
      ? undefined
      : getHelpdocTag(doc.loadStatus ?? 0, Boolean(doc.deleted), doc.group ?? ''),
  };
}

export function summarizeHelpdocDelete(tree: HelpDocTreeOption[], keys: Array<string | number>) {
  const selected = new Set(keys.map(String));
  const resolved = new Set<string>();
  const labels: string[] = [];

  function visit(nodes: HelpDocTreeOption[]) {
    for (const node of nodes) {
      if (selected.has(node.key)) {
        resolved.add(node.key);
        labels.push(node.raw.isDir ? `${node.label}（目录）` : node.label);
      }
      if (node.children?.length) visit(node.children);
    }
  }

  visit(tree);
  for (const key of selected) {
    if (!resolved.has(key)) labels.push(key);
  }
  return { count: keys.length, labels };
}

export function getHelpdocTextPreview(row: string) {
  const text = trim(row);
  if (text.length <= 200) return text;
  return `${text.slice(0, 151)}...`;
}

export function getHelpdocTextTooltip(row: string) {
  return trim(row);
}

export function buildHelpdocItemParams(query: HelpdocItemQueryModel) {
  return {
    pageNum: query.pageNum,
    pageSize: query.pageSize,
    id: query.id ? String(query.id) : undefined,
    group: query.group || undefined,
    from: query.from || undefined,
    title: query.title || undefined,
  };
}

export function isHelpdocUploadFileAccepted(filename: string) {
  const ext = filename.split('.').pop()?.toLowerCase();
  return ext === 'json' || ext === 'xlsx';
}

export function getHelpdocItemGroupOptions(tree: HelpDoc[] | undefined) {
  return [{ label: '内置', value: 'builtin' }, ...buildHelpdocGroupOptions(tree)];
}

export function isHelpdocConfigDirty(
  current: Map<string, string[]>,
  initial: Map<string, string[]>
) {
  if (current.size !== initial.size) return true;
  for (const [key, value] of current) {
    const other = initial.get(key) ?? [];
    if (value.length !== other.length) return true;
    for (let index = 0; index < value.length; index += 1) {
      if (value[index] !== other[index]) return true;
    }
  }
  return false;
}

export function buildHelpdocConfigPayload(aliases: Map<string, string[]>) {
  return Object.fromEntries(aliases);
}

export function cloneHelpdocAliases(source: Map<string, string[]>) {
  return new Map(Array.from(source.entries(), ([key, value]) => [key, [...value]]));
}

/**
 * 帮助文档图片化渲染的配置（fork 新增）。
 *
 * 命中敏感词或正文过长时改发一张现场渲染的图片，由骰主自部署的渲染后端出图；
 * 数值填 0 表示"用后端默认值"（超时 15 秒、宽度 820px）。
 */
export type HelpdocImageRenderConfig = {
  imageRenderEnable: boolean;
  imageRenderMinLength: number;
  imageRenderMaskInImage: boolean;
  imageRenderUrl: string;
  imageRenderToken: string;
  imageRenderTimeoutSec: number;
  imageRenderWidth: number;
};

export function createDefaultHelpdocImageRender(): HelpdocImageRenderConfig {
  return {
    imageRenderEnable: false,
    imageRenderMinLength: 0,
    imageRenderMaskInImage: false,
    imageRenderUrl: '',
    imageRenderToken: '',
    imageRenderTimeoutSec: 0,
    imageRenderWidth: 0,
  };
}

/** 从接口返回的配置里挑出图片渲染相关字段（缺字段时回落默认值）。 */
export function pickHelpdocImageRender(
  source: Partial<HelpdocImageRenderConfig> | null | undefined
): HelpdocImageRenderConfig {
  const defaults = createDefaultHelpdocImageRender();
  if (!source) return defaults;
  return {
    imageRenderEnable: source.imageRenderEnable ?? defaults.imageRenderEnable,
    imageRenderMinLength: source.imageRenderMinLength ?? defaults.imageRenderMinLength,
    imageRenderMaskInImage: source.imageRenderMaskInImage ?? defaults.imageRenderMaskInImage,
    imageRenderUrl: source.imageRenderUrl ?? defaults.imageRenderUrl,
    imageRenderToken: source.imageRenderToken ?? defaults.imageRenderToken,
    imageRenderTimeoutSec: source.imageRenderTimeoutSec ?? defaults.imageRenderTimeoutSec,
    imageRenderWidth: source.imageRenderWidth ?? defaults.imageRenderWidth,
  };
}

export function isHelpdocImageRenderDirty(
  current: HelpdocImageRenderConfig,
  initial: HelpdocImageRenderConfig
): boolean {
  return JSON.stringify(current) !== JSON.stringify(initial);
}

export type HelpdocItemRow = Pick<
  HelpTextVo,
  'id' | 'group' | 'from' | 'title' | 'content' | 'packageName'
>;
