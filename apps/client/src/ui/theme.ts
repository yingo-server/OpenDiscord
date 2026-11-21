/**
 * 主题与字号缩放（§1.2 / §7.3）。
 * - localStorage 持久化；index.html 内联脚本在首帧前应用，避免闪白；
 * - 切换时仅给根节点加 200ms 过渡，不做全量 transition；
 * - 跟随系统时监听 prefers-color-scheme 变化。
 */
export type ThemePref = 'system' | 'light' | 'dark';
export type FontScale = 'standard' | 'large' | 'xlarge';

const THEME_KEY = 'ufot.theme';
const SCALE_KEY = 'ufot.fontScale';

export function getThemePref(): ThemePref {
  try {
    const value = localStorage.getItem(THEME_KEY);
    if (value === 'light' || value === 'dark') return value;
  } catch {
    /* ignore */
  }
  return 'system';
}

export function resolveTheme(pref: ThemePref): 'light' | 'dark' {
  if (pref !== 'system') return pref;
  if (typeof window === 'undefined') return 'light';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function applyTheme(pref: ThemePref): void {
  if (typeof document === 'undefined') return;
  const root = document.documentElement;
  const theme = resolveTheme(pref);
  root.classList.add('theme-transitioning');
  root.classList.toggle('dark', theme === 'dark');
  root.setAttribute('data-theme', theme);
  root.style.colorScheme = theme;
  try {
    if (pref === 'system') localStorage.removeItem(THEME_KEY);
    else localStorage.setItem(THEME_KEY, pref);
  } catch {
    /* ignore */
  }
  window.setTimeout(() => root.classList.remove('theme-transitioning'), 300);
}

export function getFontScale(): FontScale {
  try {
    const value = localStorage.getItem(SCALE_KEY);
    if (value === 'large' || value === 'xlarge') return value;
  } catch {
    /* ignore */
  }
  return 'standard';
}

export function applyFontScale(scale: FontScale): void {
  const size = scale === 'large' ? '18px' : scale === 'xlarge' ? '20px' : '16px';
  document.documentElement.style.fontSize = size;
  try {
    if (scale === 'standard') localStorage.removeItem(SCALE_KEY);
    else localStorage.setItem(SCALE_KEY, scale);
  } catch {
    /* ignore */
  }
}

/** 跟随系统主题时，系统切换实时生效；返回清理函数。 */
export function watchSystemTheme(): () => void {
  if (typeof window === 'undefined' || !window.matchMedia) return () => undefined;
  const query = window.matchMedia('(prefers-color-scheme: dark)');
  const handler = () => {
    if (getThemePref() === 'system') applyTheme('system');
  };
  query.addEventListener?.('change', handler);
  return () => query.removeEventListener?.('change', handler);
}
