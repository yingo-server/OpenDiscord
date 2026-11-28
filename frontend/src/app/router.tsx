/** 路由表：只保留可用的登录、聊天、设置三块。 */

import { Navigate, Route, Routes } from 'react-router-dom';
import { AppShell } from '@/ui/layout/AppShell';
import { LoginPage } from '@/features/auth/LoginPage';
import { ChatLayout } from '@/features/chat/ChatLayout';
import { ChatPage } from '@/features/chat/ChatPage';
import { SettingsPage } from '@/features/settings/SettingsPage';
import { EmptyState } from '@/ui/primitives/controls';
import { useAuthStore } from '@/core/store/auth';
import type { ReactElement } from 'react';

function RequireAuth({ children }: { children: ReactElement }) {
  const status = useAuthStore((s) => s.status);
  if (status === 'unknown') {
    return <div className="uf-boot" aria-busy="true" />;
  }
  if (status !== 'authenticated') {
    return <Navigate to="/login" replace />;
  }
  return children;
}

function ChatPlaceholder() {
  return (
    <EmptyState icon="chat" title="选择一个会话" description="输入现有会话 ID 或从会话列表进入聊天。" />
  );
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        element={
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        }
      >
        <Route path="/" element={<Navigate to="/chats" replace />} />
        <Route path="/chats" element={<ChatLayout />}>
          <Route index element={<ChatPlaceholder />} />
          <Route path=":convId" element={<ChatPage />} />
        </Route>
        <Route path="/me" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/chats" replace />} />
      </Route>
    </Routes>
  );
}
