import { translations } from './i18n/strings';

export type Lang = 'zh-CN' | 'en-US';

let current: Lang = 'zh-CN';

export function initLang(defaultLang: string): void {
  const stored = localStorage.getItem('ufot.lang');
  current = stored === 'en-US' || stored === 'zh-CN'
    ? stored
    : defaultLang.startsWith('en') ? 'en-US' : 'zh-CN';
  document.documentElement.lang = current;
}

export type TFunc = (key: string) => string;

export function t(key: string): string {
  const dict = translations[current];
  return dict[key] || translations['zh-CN'][key] || key;
}
