/**
 * 媒体上传（后端只有 HTTP 面，WS presign 未实现）。
 *
 * 后端事实（docs/client-contract.md §4）：
 * - POST /api/media/upload，multipart，字段名 file，字段名写错会 400；
 * - 图片 8MB / 音频 2MB / 文件 20MB；魔数白名单 JPEG/PNG/WEBP/GIF/OGG/MP3；
 * - 单用户每日 50 次；同字节重复上传返回 200（内容寻址）。
 * - 服务端会用自己的魔数判定覆盖客户端声明的 mime，因此本地只做**前置预检**。
 */

import { env } from '@/core/config/env';
import { request } from '@/core/net/http';
import type { MediaItem } from './types';

export const MEDIA_LIMITS = {
  image: 8 * 1024 * 1024,
  audio: 2 * 1024 * 1024,
  file: 20 * 1024 * 1024,
} as const;

export interface UploadResult {
  media_id: string;
  mime: string;
  mtype: number;
  size: number;
}

const EXTENSION_MIME: Record<string, string> = {
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  png: 'image/png',
  webp: 'image/webp',
  gif: 'image/gif',
  ogg: 'audio/ogg',
  oga: 'audio/ogg',
  mp3: 'audio/mpeg',
};

/** 上传前本地预检：大小上限 + 扩展名与白名单。返回可展示的错误文案。 */
export function precheckFile(file: File): string | null {
  const ext = file.name.split('.').pop()?.toLowerCase() ?? '';
  if (!EXTENSION_MIME[ext]) {
    return `不支持的文件类型（.${ext}）。仅支持 ${Object.keys(EXTENSION_MIME).join(' / ')}。`;
  }
  const limit = file.type.startsWith('image/')
    ? MEDIA_LIMITS.image
    : file.type.startsWith('audio/')
      ? MEDIA_LIMITS.audio
      : MEDIA_LIMITS.file;
  if (file.size > limit) {
    return `文件超过上限（${Math.round(limit / 1024 / 1024)}MB）。`;
  }
  return null;
}

/** 上传单个文件，返回服务端登记的媒体条目。 */
export async function uploadMedia(file: File): Promise<MediaItem> {
  const form = new FormData();
  form.append('file', file, file.name);
  const body = await request<UploadResult>('/api/media/upload', {
    method: 'POST',
    base: env.apiBase,
    body: form,
  });
  return {
    mediaId: body.media_id,
    mtype: body.mtype,
    mime: body.mime,
    size: body.size,
    name: file.name,
  };
}

/** 媒体读取地址（需带鉴权，浏览器直接 <img src> 不带 Cookie → 用 fetch + blob URL）。 */
export function mediaUrl(mediaId: string): string {
  return `${env.apiBase}/api/media/${encodeURIComponent(mediaId)}`;
}

/** 拉取媒体字节（跨源部署时必须走 fetch + Authorization）。 */
export async function fetchMediaBlob(mediaId: string): Promise<Blob> {
  return request<Blob>(`/api/media/${encodeURIComponent(mediaId)}`, {
    base: env.apiBase,
    expect: 'blob',
  });
}
