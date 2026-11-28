import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { HashRouter } from 'react-router-dom';
import { AppRoutes } from '@/app/router';
import { Boot } from '@/app/boot';
import { ToastStack, useToastAutoDismiss } from '@/ui/primitives/Toast';
import { useUiStore } from '@/core/store/ui';

import '@/styles/tokens.css';
import '@/styles/base.css';
import '@/styles/ui.css';
import '@/styles/layout.css';
import '@/styles/features.css';

function Toasts() {
  const toasts = useUiStore((s) => s.toasts);
  const dismiss = useUiStore((s) => s.dismissToast);
  useToastAutoDismiss(toasts, dismiss);
  return <ToastStack items={toasts} />;
}

const container = document.getElementById('root');
if (!container) throw new Error('缺少 #root 容器');

createRoot(container).render(
  <StrictMode>
    <HashRouter>
      <Boot>
        <AppRoutes />
      </Boot>
      <Toasts />
    </HashRouter>
  </StrictMode>,
);
