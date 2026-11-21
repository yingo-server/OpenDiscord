/**
 * 登录应答契约测试：mapAuthSession 只认 authsvc sessionResponse 的
 * snake_case 形态（server/internal/authsvc/tokens.go:27 唯一权威），字段集
 * 锁死：user_id/device_id/access_token/token/refresh_token/expires_at/
 * expires_at_unix/session_id/guest/is_new_user/profile{display_name}。
 * mockBridge session() 返回同形应答 —— 两者走同一条 mapAuthSession 路径。
 */
import { describe, expect, it } from 'vitest';
import { AuthError, mapAuthSession, type RawAuthSession } from './provider';
import { createMockAuthApi } from '../mock/mockBridge';

describe('mapAuthSession（snake_case 单态）', () => {
  const raw: RawAuthSession = {
    user_id: 1001,
    device_id: 'dev-abc',
    access_token: 'at-1',
    token: 'at-1',
    refresh_token: 'rt-1',
    expires_at: 1_800_000_000_000,
    expires_at_unix: 1_800_000_000,
    session_id: 'dev-abc', // tokens.go:74 SessionID = deviceID
    guest: false,
    is_new_user: true,
    profile: { display_name: '我' },
  };

  it('完整 sessionResponse → AuthSession 领域对象', () => {
    const s = mapAuthSession(raw);
    expect(s).toEqual({
      token: 'at-1',
      refreshToken: 'rt-1',
      userId: 1001,
      deviceId: 'dev-abc',
      isNewUser: true,
      sessionId: 'dev-abc',
      expiresAt: 1_800_000_000_000,
      guest: false,
      profile: { displayName: '我', avatar: undefined },
    });
  });

  it('token 与 access_token 任一存在即可；缺失报错', () => {
    expect(mapAuthSession({ ...raw, token: undefined }).token).toBe('at-1');
    expect(mapAuthSession({ ...raw, access_token: undefined }).token).toBe('at-1');
    expect(() => mapAuthSession({ ...raw, token: undefined, access_token: undefined })).toThrow(
      AuthError,
    );
  });

  it('camelCase 双形态兜底已删除（P1-2 简化）：camel 输入按缺失处理', () => {
    const camelOnly = {
      token: 't',
      userId: 1001,
      sessionId: 's',
      expiresAt: 1,
      refreshToken: 'r',
      profile: { displayName: 'x' },
    } as unknown as RawAuthSession;
    const s = mapAuthSession(camelOnly);
    expect(s.userId).toBe(0);
    expect(s.sessionId).toBe('');
    expect(s.profile).toEqual({ displayName: undefined, avatar: undefined });
  });
});

describe('mockBridge 登录应答（与 authsvc 同形）', () => {
  it('guest 登录返回完整 snake_case sessionResponse，session_id = device_id', async () => {
    const api = createMockAuthApi();
    const raw = (await api.post<Record<string, unknown>>('/api/auth/guest', {
      nickname: '游客',
    })) as Record<string, unknown>;
    // 字段集与 tokens.go sessionResponse 逐字段对齐
    expect(Object.keys(raw).sort()).toEqual(
      [
        'user_id',
        'device_id',
        'access_token',
        'token',
        'refresh_token',
        'expires_at',
        'expires_at_unix',
        'session_id',
        'guest',
        'is_new_user',
        'profile',
      ].sort(),
    );
    expect(raw.session_id).toBe(raw.device_id); // sessionId 语义对齐真实端
    expect(raw.is_new_user).toBe(true);
    expect(raw.guest).toBe(true);
    expect(raw.expires_at_unix).toBe(Math.floor(Number(raw.expires_at) / 1000));
    const s = mapAuthSession(raw as RawAuthSession);
    expect(s.token).toBe(raw.access_token);
    expect(s.sessionId).toBe(raw.device_id);
  });

  it('密码登录同样返回 sessionResponse 形态', async () => {
    const api = createMockAuthApi();
    const raw = (await api.post<Record<string, unknown>>('/api/auth/password', {
      account: 'a@b.c',
      password: 'ufot',
    })) as Record<string, unknown>;
    expect(raw.user_id).toBeDefined();
    expect(raw.session_id).toBe(raw.device_id);
  });
});
