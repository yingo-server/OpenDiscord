/** 启动引导：恢复会话 → 探测登录方式 → 建立 WebSocket。 */

import { useEffect } from 'react';
import { useAuthStore } from '@/core/store/auth';
import { connectWs, disconnectWs } from '@/core/store/bridge';
import { initTheme } from '@/core/store/ui';

export function Boot({ children }: { children: React.ReactNode }) {
  const status = useAuthStore((s) => s.status);
  const token = useAuthStore((s) => s.session?.access_token ?? null);

  useEffect(() => {
    initTheme();
    void useAuthStore.getState().restore();
    void useAuthStore.getState().loadMethods();
  }, []);

  useEffect(() => {
    if (status === 'authenticated' && token) {
      connectWs(token);
    } else {
      disconnectWs();
    }
    return () => disconnectWs();
  }, [status, token]);

  return <>{children}</>;
}
