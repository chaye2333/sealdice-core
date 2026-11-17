import { client } from './generated/client.gen';
import { useAuthStore } from '@/features/auth/store';
import { appPinia } from '@/pinia';

/**
 * v1 接口（`/sd-api/*`，echo 风格，没有进 OpenAPI）的通用请求封装。
 *
 * ⚠️ 鉴权注意：v1 的 `doAuth` 读的是 **`token` 头**（或 `?token=` 查询参数），
 * 不是 `Authorization: Bearer`。所以两个头都带上：前者给 v1，后者给统一拦截器。
 *
 * `formData` 存在时按 multipart 发送（浏览器自己补 boundary），否则把 `body` 当 JSON。
 */
type Envelope = { result?: boolean; err?: string };

export type V1RequestMethod = 'get' | 'post' | 'delete';

export type V1RequestOptions = {
  params?: Record<string, unknown>;
  body?: unknown;
  formData?: FormData;
  /** 服务端没有给 err 时的兜底文案。 */
  fallbackError?: string;
};

function authHeaders(): Record<string, string> {
  const token = useAuthStore(appPinia).token;
  if (!token) return {};
  return { token, Authorization: `Bearer ${token}` };
}

export async function requestV1<T>(
  method: V1RequestMethod,
  url: string,
  options: V1RequestOptions = {}
): Promise<T> {
  const response = await client.instance.request<Envelope & T>({
    method,
    url,
    params: options.params,
    data: options.formData ?? options.body,
    headers: authHeaders(),
  });
  const data = response.data as (Envelope & T) | undefined;
  if (!data || data.result === false) {
    throw new Error(data?.err || options.fallbackError || '请求失败');
  }
  return data;
}
