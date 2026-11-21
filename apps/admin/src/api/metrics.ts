import { getText } from './client';

/**
 * Prometheus 文本格式解析（/metrics，expfmt 0.0.4）。
 * 管理后台只读展示，不引入 SDK —— 解析 50 行代码足够，也便于断网时静态兜底。
 */
export interface MetricSample {
  name: string;
  labels: Record<string, string>;
  value: number;
}

export interface MetricFamily {
  name: string;
  help: string;
  type: string;
  samples: MetricSample[];
}

function parseLabels(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  const re = /([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(raw)) !== null) {
    out[m[1]!] = m[2]!.replace(/\\"/g, '"').replace(/\\\\/g, '\\').replace(/\\n/g, '\n');
  }
  return out;
}

function parseSampleLine(line: string): MetricSample | null {
  let i = 0;
  while (i < line.length && !/[\s{]/.test(line[i]!)) i += 1;
  const name = line.slice(0, i);
  if (!name) return null;

  let labels: Record<string, string> = {};
  let rest = line.slice(i).trimStart();

  if (rest.startsWith('{')) {
    let depth = 0;
    let end = -1;
    for (let j = 0; j < rest.length; j += 1) {
      const ch = rest[j]!;
      if (ch === '{') depth += 1;
      else if (ch === '}') {
        depth -= 1;
        if (depth === 0) {
          end = j;
          break;
        }
      }
    }
    if (end < 0) return null;
    labels = parseLabels(rest.slice(1, end));
    rest = rest.slice(end + 1).trimStart();
  }

  const valueToken = rest.split(/\s+/)[0];
  if (!valueToken) return null;
  const value = Number(valueToken);
  if (Number.isNaN(value)) return null;
  return { name, labels, value };
}

export function parseMetrics(text: string): MetricFamily[] {
  const families = new Map<string, MetricFamily>();
  let currentHelp = '';
  let currentType = '';

  for (const rawLine of text.split('\n')) {
    const line = rawLine.trim();
    if (!line) continue;
    if (line.startsWith('#')) {
      const help = /^#\s*HELP\s+(\S+)\s?(.*)$/.exec(line);
      if (help) {
        currentHelp = help[2] ?? '';
        const fam = families.get(help[1]!);
        if (fam) fam.help = currentHelp;
        else families.set(help[1]!, { name: help[1]!, help: currentHelp, type: 'untyped', samples: [] });
        continue;
      }
      const type = /^#\s*TYPE\s+(\S+)\s+(\S+)/.exec(line);
      if (type) {
        currentType = type[2]!;
        const fam = families.get(type[1]!);
        if (fam) fam.type = currentType;
        else families.set(type[1]!, { name: type[1]!, help: '', type: currentType, samples: [] });
        continue;
      }
      continue;
    }

    const sample = parseSampleLine(line);
    if (!sample) continue;
    // histogram/summary 的 _bucket/_sum/_count 归到基名家族，便于按 label 聚合
    const base = sample.name.replace(/_(bucket|sum|count)$/, (m) => (m === '_bucket' ? '_bucket' : m));
    const family = families.get(base) ?? { name: base, help: currentHelp, type: currentType || 'untyped', samples: [] };
    family.samples.push({ ...sample, name: base });
    families.set(base, family);
  }

  return [...families.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/** 指标端点在管理 API 之外，走独立监听；两处都试，便于同域/独立域部署。 */
export async function fetchMetricsText(signal?: AbortSignal): Promise<string> {
  const errors: string[] = [];
  for (const path of ['/metrics', '/metrics/internal']) {
    try {
      return await getText(path, signal);
    } catch (err) {
      errors.push((err as Error).message);
    }
  }
  throw new Error(`无法读取 /metrics：${errors.join('; ')}`);
}

/* --------------------------- 聚合辅助 --------------------------- */

export function sumMetric(families: MetricFamily[], name: string, match?: Record<string, string>): number | null {
  const fam = families.find((f) => f.name === name);
  if (!fam) return null;
  let total = 0;
  let hit = false;
  for (const s of fam.samples) {
    if (match && !Object.entries(match).every(([k, v]) => s.labels[k] === v)) continue;
    total += s.value;
    hit = true;
  }
  return hit ? total : null;
}

export function maxMetric(families: MetricFamily[], name: string): number | null {
  const fam = families.find((f) => f.name === name);
  if (!fam || fam.samples.length === 0) return null;
  return Math.max(...fam.samples.map((s) => s.value));
}

/** 第一个存在的指标名即采用（服务端演进时前端不必同步改）。 */
export function firstMetric(
  families: MetricFamily[],
  names: string[],
  mode: 'sum' | 'max' = 'sum',
): { name: string; value: number } | null {
  for (const name of names) {
    const value = mode === 'sum' ? sumMetric(families, name) : maxMetric(families, name);
    if (value !== null) return { name, value };
  }
  return null;
}

/** HTTP 错误率：5xx / 全部请求；无数据返回 null（显示 "—" 而不是 0）。 */
export function httpErrorRate(families: MetricFamily[]): number | null {
  const fam =
    families.find((f) => f.name === 'ufot_http_requests_total') ??
    families.find((f) => f.name === 'gateway_requests_total');
  if (!fam || fam.samples.length === 0) return null;
  let total = 0;
  let bad = 0;
  for (const s of fam.samples) {
    total += s.value;
    const status = s.labels.status ?? s.labels.code ?? '';
    if (status.startsWith('5')) bad += s.value;
  }
  return total > 0 ? bad / total : null;
}
