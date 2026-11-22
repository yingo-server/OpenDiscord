/** kind 940/941 资料更新（唯一有效路径；450 服务端不处理）。 */
import { K } from '../protocol/kinds';
import type { WsRequests } from './requests';
import { parseIdentityResult } from './requests';

export interface StoreUser {
  uid: number;
  displayName?: string;
  avatarMediaID?: number;
  status?: number;
}

export class IdentityApi {
  constructor(private readonly requests: WsRequests) {}

  async updateProfile(input: { display_name: string; avatar_url?: string }): Promise<void> {
    const body: StoreUser = { uid: 0, displayName: input.display_name };
    await this.requests.request(K.IDENTITY_REQUEST, {
      op: 1,
      requestId: `profile-${Date.now()}`,
      body: btoa(JSON.stringify(body)),
    }, 10_000);
  }

  async getProfile(userId: number): Promise<StoreUser | null> {
    const result = await this.requests.request(K.IDENTITY_REQUEST, {
      op: 2,
      userId,
      requestId: `profile-get-${Date.now()}`,
    }, 10_000);
    return parseIdentityResult(result.payload) as unknown as StoreUser | null;
  }
}
