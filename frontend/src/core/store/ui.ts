/** UI 状态：主题、轻提示。 */

import { create } from 'zustand';
import type { ToastItem, ToastTone } from '@/ui/primitives/Toast';

export type ThemeMode = 'dark' | 'light' | 'system';

const THEME_KEY = 'ufot.theme';

function loadTheme(): ThemeMode {
  try {
    const raw = localStorage.getItem(THEME_KEY);
    if (raw === 'dark' || raw === 'light' || raw === 'system') return raw;
  } catch {
    /* 忽略 */
  }
  return 'dark';
}

function applyTheme(mode: ThemeMode): void {
  const prefersLight =
    typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: light)').matches;
  const resolved = mode === 'system' ? (prefersLight ? 'light' : 'dark') : mode;
  document.documentElement.dataset.theme = resolved;
}

interface UiState {
  theme: ThemeMode;
  toasts: ToastItem[];
  setTheme: (mode: ThemeMode) => void;
  pushToast: (text: string, tone?: ToastTone) => void;
  dismissToast: (id: number) => void;
}

let toastSeed = 1;

export const useUiStore = create<UiState>((set, get) => ({
  theme: loadTheme(),
  toasts: [],
  setTheme(mode) {
    try {
      localStorage.setItem(THEME_KEY, mode);
    } catch {
      /* 忽略 */
    }
    applyTheme(mode);
    set({ theme: mode });
  },
  pushToast(text, tone = 'info') {
    const id = toastSeed++;
    set({ toasts: [...get().toasts, { id, text, tone }].slice(-3) });
  },
  dismissToast(id) {
    set({ toasts: get().toasts.filter((t) => t.id !== id) });
  },
}));

/** 启动时应用主题并监听系统主题变化。 */
export function initTheme(): void {
  applyTheme(useUiStore.getState().theme);
  if (typeof matchMedia === 'function') {
    matchMedia('(prefers-color-scheme: light)').addEventListener('change', () => {
      if (useUiStore.getState().theme === 'system') applyTheme('system');
    });
  }
}
