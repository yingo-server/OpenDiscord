export type ThemeMode = 'light' | 'dark' | 'system';

const KEY = 'ufot.theme';

export function currentTheme(): ThemeMode {
  const value = localStorage.getItem(KEY);
  return value === 'light' || value === 'dark' || value === 'system' ? value : 'system';
}

function prefersDark(): boolean {
  return window.matchMedia('(prefers-color-scheme: dark)').matches;
}

export function applyTheme(mode: ThemeMode): void {
  const dark = mode === 'dark' || (mode === 'system' && prefersDark());
  const root = document.documentElement;
  root.classList.toggle('dark', dark);
  root.setAttribute('data-theme', dark ? 'dark' : 'light');
  root.style.colorScheme = dark ? 'dark' : 'light';
  localStorage.setItem(KEY, mode);
}

export function initTheme(): () => void {
  const media = window.matchMedia('(prefers-color-scheme: dark)');
  const onChange = () => {
    if (currentTheme() === 'system') applyTheme('system');
  };
  applyTheme(currentTheme());
  media.addEventListener('change', onChange);
  return () => media.removeEventListener('change', onChange);
}

export function clearLocalData(): void {
  for (const key of Object.keys(localStorage)) {
    if (key.startsWith('ufot.cache.') || key.startsWith('ufot.draft.') || key === 'ufot.lang') {
      localStorage.removeItem(key);
    }
  }
}
