/**
 * 管理后台 i18n：key → 文案，{name} 具名占位符。
 * 字符串统一在 zh-CN.ts 维护；组件不写中文常量，不拼接翻译片段。
 */
import { zhCN, type AdminI18nKey } from './zh-CN';

export type { AdminI18nKey };

const catalogs: Record<string, Record<string, string>> = {
  'zh-CN': zhCN,
};

let locale = 'zh-CN';

export function setLocale(next: string): void {
  if (catalogs[next]) locale = next;
}

export function getLocale(): string {
  return locale;
}

/** 静态文案用 AdminI18nKey 获得编译期校验；动态后端 key 用 string 兜底。 */
export function t(key: AdminI18nKey | string, params?: Record<string, string | number>): string {
  const template = catalogs[locale]?.[key] ?? catalogs['zh-CN']?.[key] ?? key;
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (match, name: string) =>
    params[name] === undefined ? match : String(params[name]),
  );
}
