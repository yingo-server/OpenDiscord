/**
 * SW 缓存策略判定（纯逻辑）——public/sw.js 内联实现的镜像与单测对象。
 * 修改任一侧必须同步另一侧（sw.js 是构建期原样复制，不能 import 本模块）。
 */

export type CacheDecision =
  | 'bypass' // /api/、/ws：交给浏览器，绝不缓存
  | 'shell' // 静态壳：cache-first，miss 网络并回填
  | 'media' // 已缓存媒体：命中回缓存，miss 网络并回填
  | 'pass'; // 其它：默认网络

export function shouldBypassCache(pathname: string): boolean {
  return pathname.startsWith('/api/') || pathname.startsWith('/ws');
}

export function isShellAsset(pathname: string, method: string, sameOrigin: boolean): boolean {
  if (method !== 'GET' || !sameOrigin) return false;
  if (pathname === '/' || pathname === '/index.html') return true;
  if (pathname === '/manifest.webmanifest') return true;
  if (pathname.startsWith('/icons/')) return true;
  if (pathname.startsWith('/assets/')) return true;
  return /\.(css|js|mjs|woff2?|ttf|otf|png|svg|ico)$/.test(pathname);
}

export function isMediaAsset(pathname: string, method: string): boolean {
  return method === 'GET' && pathname.startsWith('/api/media/');
}

export function decide(pathname: string, method: string, sameOrigin: boolean): CacheDecision {
  if (shouldBypassCache(pathname)) return 'bypass';
  if (isShellAsset(pathname, method, sameOrigin)) return 'shell';
  if (isMediaAsset(pathname, method)) return 'media';
  return 'pass';
}
