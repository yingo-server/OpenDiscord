/**
 * Dashboard / Sparkline 冒烟测试（node:test + react-dom/server SSR）。
 * 用 fetch stub 模拟后端指标，验证卡片渲染与健康矩阵/sparkline 点数逻辑。
 */
import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';

const SAMPLE_METRICS = [
  '# HELP ufot_admin_build_info build',
  '# TYPE ufot_admin_build_info gauge',
  'ufot_admin_build_info{version="dev"} 1',
  '# TYPE go_goroutines gauge',
  'go_goroutines 27',
  '# TYPE process_resident_memory_bytes gauge',
  'process_resident_memory_bytes 104857600',
  '# TYPE ufot_admin_uptime_seconds gauge',
  'ufot_admin_uptime_seconds 7200',
  '# TYPE ufot_admin_audit_append_failures_total counter',
  'ufot_admin_audit_append_failures_total 0',
  '# TYPE ufot_admin_bridge_requests_total counter',
  'ufot_admin_bridge_requests_total 100',
  '# TYPE ufot_admin_bridge_failures_total counter',
  'ufot_admin_bridge_failures_total 2',
  '# TYPE ufot_admin_bridge_circuit_open gauge',
  'ufot_admin_bridge_circuit_open 0',
  '# TYPE ufot_admin_login_failures_total counter',
  'ufot_admin_login_failures_total 5',
  '# TYPE ufot_ws_connections_active gauge',
  'ufot_ws_connections_active 42',
  '# TYPE ufot_users_total counter',
  'ufot_users_total 900',
  '# TYPE ufot_users_created_today_total counter',
  'ufot_users_created_today_total 7',
  '# TYPE ufot_messages_today_total counter',
  'ufot_messages_today_total 3100',
].join('\n');

const SAMPLE_ARCHIVE = {
  running: false,
  current_run: null,
  last_run: { id: 1, started_at: '2026-01-01T00:00:00Z', finished_at: null, moved_rows: 5, convs_touched: 1, status: 'ok', dry_run: false },
  next_run_at: '2026-01-02T00:00:00Z',
  hot_rows: 10,
  hot_bytes: 100,
  oldest_message_at: null,
  lag_days: 0,
  policy: { hot_retention_days: 30, cron: '0 3 * * *', batch_size: 1000 },
  cold_db: { reachable: true, latency_ms: 12 },
};

const SAMPLE_AUDIT = {
  events: [
    { id: 3, ts: '2026-01-01T08:00:03Z', type: 'report.handle', actor: { id: '1', username: 'op' }, summary: '处理举报', payload: {} },
    { id: 2, ts: '2026-01-01T08:00:02Z', type: 'user.ban', actor: { id: '1', username: 'op' }, summary: '封禁用户', payload: {} },
    { id: 1, ts: '2026-01-01T08:00:01Z', type: 'flag.set', actor: { id: '1', username: 'op' }, summary: '改 flag', payload: {} },
  ],
  next_since_id: 3,
  first_id: 1,
  has_more: false,
};

function installDom() {
  const listeners: Record<string, Array<() => void>> = {};
  (globalThis as Record<string, unknown>).window = {
    matchMedia: () => ({ matches: true }),
    AudioContext: undefined,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
  };
  (globalThis as Record<string, unknown>).localStorage = { getItem: () => null, setItem: () => {} };
  (globalThis as Record<string, unknown>).sessionStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
  (globalThis as Record<string, unknown>).document = {
    hidden: true, // 保持隐藏，避免 useEffect 里拉取/定时器真正跑
    visibilityState: 'hidden',
    addEventListener: (t: string, fn: () => void) => void (listeners[t] ??= []).push(fn),
    removeEventListener: () => {},
    createElement: () => ({ style: {}, setAttribute: () => {} }),
    title: '',
  };
  (globalThis as Record<string, unknown>).fetch = async (url: string) => {
    const u = String(url);
    if (u.includes('/metrics')) return { ok: true, text: async () => SAMPLE_METRICS };
    if (u.includes('/archive/status')) return { ok: true, json: async () => SAMPLE_ARCHIVE };
    if (u.includes('/reports')) return { ok: true, json: async () => ({ items: [], total: 4, page: 1, per_page: 1 }) };
    if (u.includes('/audit')) return { ok: true, json: async () => SAMPLE_AUDIT };
    return { ok: false, status: 404, text: async () => '', json: async () => ({}) };
  };
  return listeners;
}

beforeEach(() => {
  installDom();
});

test('Dashboard 冒烟：SSR 渲染出指标卡与健康矩阵', async () => {
  const { Dashboard } = await import('./Dashboard');
  const React = (await import('react')) as typeof import('react');
  const { renderToString } = (await import('react-dom/server')) as typeof import('react-dom/server');
  const html = renderToString(React.createElement(Dashboard));
  assert.ok(html.includes('仪表盘'), '标题渲染');
  assert.ok(html.includes('在线 WS 连接'));
  assert.ok(html.includes('总用户数'));
  assert.ok(html.includes('今日消息数'));
  assert.ok(html.includes('举报待处理'));
  assert.ok(html.includes('健康矩阵'));
  assert.ok(html.includes('实时事件流'));
  // 健康矩阵：DB integrity 诚实标注
  assert.ok(html.includes('ufot_db_integrity_ok'), 'DB integrity 缺失指标标注');
});

test('Sparkline：点数不足显示采样提示，有数据时输出 polyline', async () => {
  const { Sparkline } = await import('../components/Sparkline');
  const React = (await import('react')) as typeof import('react');
  const { renderToString } = (await import('react-dom/server')) as typeof import('react-dom/server');
  const empty = renderToString(React.createElement(Sparkline, { points: [{ t: 1, value: 1 }] }));
  assert.ok(empty.includes('采样中'));
  const pts = Array.from({ length: 60 }, (_, i) => ({ t: i * 1000, value: i }));
  const full = renderToString(React.createElement(Sparkline, { points: pts, ariaLabel: '连接趋势' }));
  assert.ok(full.includes('<polyline'), '折线渲染');
  const segments = full.split(' ').filter((s) => s.includes(',')).length;
  assert.ok(segments >= 60, `坐标点数 >= 60（实际 ${segments}）`);
  assert.ok(full.includes('aria-label="连接趋势"'));
});

test('指标解析接线：样例 metrics 的关键字段可被卡片读到', async () => {
  const { parseMetrics, sumMetric, firstMetric } = await import('../../api/metrics');
  const fams = parseMetrics(SAMPLE_METRICS);
  assert.equal(firstMetric(fams, ['ufot_ws_connections_active'])!.value, 42);
  assert.equal(sumMetric(fams, 'ufot_users_total'), 900);
  assert.equal(sumMetric(fams, 'ufot_users_created_today_total'), 7);
  assert.equal(sumMetric(fams, 'ufot_messages_today_total'), 3100);
  assert.equal(sumMetric(fams, 'ufot_admin_audit_append_failures_total'), 0);
});
