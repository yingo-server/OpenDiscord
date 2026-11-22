import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { bootstrap } from './app/bootstrap';
import { App } from './ui/App';
import { ErrorBoundary } from './ui/components/ErrorBoundary';
import { ToastHost } from './ui/components/Toast';
import { env } from './platform/env';
import { initLang } from './ui/i18n';
import { initTheme } from './platform/theme';
import './ui/styles/index.css';

initLang(env.defaultLang);
initTheme();
bootstrap();

const container = document.getElementById('root');
if (!container) throw new Error('Root element missing');

createRoot(container).render(
  <StrictMode>
    <ErrorBoundary>
      <App />
      <ToastHost />
    </ErrorBoundary>
  </StrictMode>,
);
