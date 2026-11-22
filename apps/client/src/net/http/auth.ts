import { HttpClient, ApiError } from './client';
import type { Session } from '../../domain/types';
import { deviceId } from '../../platform/session';

export interface CodeResult {
  ok?: boolean;
  sent?: boolean;
  challenge_id?: string;
  expires_in?: number;
  resend_after?: number;
  poll_after?: number;
}

export class AuthApi {
  constructor(private readonly http: HttpClient) {}

  phoneCode(phone: string): Promise<CodeResult> {
    return this.http.post<CodeResult>('/api/auth/phone/code', { phone });
  }
  phoneVerify(phone: string, code: string, device?: string): Promise<Session> {
    return this.http.post<Session>('/api/auth/phone/verify', { phone, code, device_id: device || deviceId() });
  }
  emailLink(email: string): Promise<CodeResult> {
    return this.http.post<CodeResult>('/api/auth/email/link', { email });
  }
  emailPoll(email: string, device?: string): Promise<Session | { status: string }> {
    return this.http.post<Session | { status: string }>('/api/auth/email/poll', { email, device_id: device || deviceId() });
  }
  password(account: string, password: string, device?: string): Promise<Session> {
    return this.http.post<Session>('/api/auth/password', { account, password, device_id: device || deviceId() });
  }
  guest(nickname?: string, device?: string): Promise<Session> {
    return this.http.post<Session>('/api/auth/guest', { nickname: nickname || undefined, device_id: device || deviceId() });
  }
  refresh(refreshToken: string, device?: string): Promise<Session> {
    return this.http.post<Session>('/api/auth/refresh', { refresh_token: refreshToken, device_id: device || deviceId() });
  }
  logout(refreshToken: string): Promise<{ ok?: boolean }> {
    return this.http.post<{ ok?: boolean }>('/api/auth/logout', { refresh_token: refreshToken });
  }
  setPassword(password: string): Promise<{ ok?: boolean }> {
    return this.http.post<{ ok?: boolean }>('/api/auth/password/set', { password });
  }
  updateProfile(payload: { display_name?: string; avatar_url?: string }): Promise<unknown> {
    return this.http.post<unknown>('/api/profile/update', payload);
  }
}

export interface UploadedMedia { id?: string; media_id?: string; }

export class MediaApi {
  constructor(private readonly http: HttpClient) {}

  async upload(file: File): Promise<string> {
    const form = new FormData();
    form.append('file', file, file.name);
    const result = await this.http.request<UploadedMedia>('/api/media/upload', { method: 'POST', body: form }, 60_000);
    const id = result.id || result.media_id;
    if (!id) throw new ApiError('UPLOAD_FAILED', '上传未完成，请重试');
    return id;
  }

  url(id: string): string {
    return `${this.http.endpoint}/api/media/${encodeURIComponent(id)}`;
  }
}
