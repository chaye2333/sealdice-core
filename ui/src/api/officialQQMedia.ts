import { requestV1 } from './v1Request';

/** 可管理素材的官方 QQ 机器人（对应一个 endpoint）。 */
export type OfficialQQMediaBot = {
  id: string;
  name: string;
  appId: string;
};

/** 一张已上传的 Markdown 图片素材。width/height 是后端上传时补记的，可能缺失。 */
export type OfficialQQMediaImage = {
  imageId: string;
  /** qqbot-media://image/v1/<token> */
  url: string;
  fileName: string;
  width?: number;
  height?: number;
  createdAt?: number;
  expiresAt?: number;
  /** 平台给的短时效预览地址，只用于展示，不要持久化。 */
  previewUrl?: string;
};

export type OfficialQQMediaList = {
  bots: OfficialQQMediaBot[];
  endpointId: string;
  images: OfficialQQMediaImage[];
  total: number;
  nextCursor: string;
};

export async function fetchOfficialQQMediaImages(
  params: { endpointId?: string; cursor?: string } = {}
): Promise<OfficialQQMediaList> {
  const data = await requestV1<{
    bots?: OfficialQQMediaBot[];
    endpointId?: string;
    images?: OfficialQQMediaImage[];
    total?: number;
    nextCursor?: string;
  }>('get', '/sd-api/official-qq/media-images', {
    params: { endpointId: params.endpointId || undefined, cursor: params.cursor || undefined },
    fallbackError: '加载图片列表失败',
  });
  return {
    bots: (data.bots ?? []).filter(bot => !!bot?.id),
    endpointId: data.endpointId ?? '',
    images: (data.images ?? []).filter(item => !!item?.imageId && !!item?.url),
    total: data.total ?? 0,
    nextCursor: data.nextCursor ?? '',
  };
}

export async function uploadOfficialQQMediaImage(
  endpointId: string,
  file: File
): Promise<{ image: OfficialQQMediaImage; endpointId: string }> {
  const formData = new FormData();
  if (endpointId) formData.append('endpointId', endpointId);
  formData.append('file', file, file.name);
  const data = await requestV1<{ image?: OfficialQQMediaImage; endpointId?: string }>(
    'post',
    '/sd-api/official-qq/media-images',
    { formData, fallbackError: '上传图片失败' }
  );
  if (!data.image) throw new Error('上传失败：服务端没有返回图片信息');
  return { image: data.image, endpointId: data.endpointId ?? endpointId };
}

export async function deleteOfficialQQMediaImage(
  endpointId: string,
  imageId: string
): Promise<void> {
  await requestV1('delete', `/sd-api/official-qq/media-images/${encodeURIComponent(imageId)}`, {
    params: { endpointId: endpointId || undefined },
    fallbackError: '删除图片失败',
  });
}
