/** 路由表：/login 独立；/admin 独立外壳；其余走用户外壳。 */

import { Navigate, Route, Routes } from 'react-router-dom';
import { AppShell } from '@/ui/layout/AppShell';
import { LoginPage } from '@/features/auth/LoginPage';
import { ChatLayout } from '@/features/chat/ChatLayout';
import { ChatPage } from '@/features/chat/ChatPage';
import { ContactsPage } from '@/features/contacts/ContactsPage';
import { SettingsPage } from '@/features/settings/SettingsPage';
import { AdminApp } from '@/features/admin/AdminApp';
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
    <EmptyState
      icon="chat"
      title="选择一个会话"
      description="从左侧列表选择会话开始聊天。移动端请使用底部标签栏返回列表。"
    />
  );
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/admin/*" element={<AdminApp />} />
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
        <Route path="/contacts" element={<ContactsPage />} />
        <Route path="/me" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/chats" replace />} />
      </Route>
    </Routes>
  );
}
