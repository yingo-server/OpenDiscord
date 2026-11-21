import { test } from 'node:test';
import assert from 'node:assert/strict';

/** client.ts 在模块顶层读 sessionStorage —— 必须在 import 前打桩（ESM import 会提升）。 */
(globalThis as Record<string, unknown>).sessionStorage = {
  getItem: () => null,
  setItem: () => {},
  removeItem: () => {},
};

const { parseMetrics, sumMetric, maxMetric, firstMetric, httpErrorRate } = await import('./metrics');

test('解析基础 counter/gauge 与 HELP/TYPE', () => {
  const text = [
    '# HELP ufot_users_total Total users.',
    '# TYPE ufot_users_total counter',
    'ufot_users_total 1234',
    '# HELP go_goroutines Number of goroutines.',
    '# TYPE go_goroutines gauge',
    'go_goroutines 42',
  ].join('\n');
  const fams = parseMetrics(text);
  assert.equal(fams.length, 2);
  const users = fams.find((f) => f.name === 'ufot_users_total')!;
  assert.equal(users.type, 'counter');
  assert.equal(users.help, 'Total users.');
  assert.equal(users.samples[0]!.value, 1234);
});

test('标签解析（含转义引号）', () => {
  const text = 'ufot_admin_http_requests_total{method="GET",status="200"} 10\n'
    + 'ufot_admin_http_requests_total{method="POST",reason="say \\"hi\\""} 3\n';
  const fams = parseMetrics(text);
  const s = fams[0]!.samples;
  assert.equal(s[0]!.labels.method, 'GET');
  assert.equal(s[0]!.labels.status, '200');
  assert.equal(s[1]!.labels.reason, 'say "hi"');
});

test('多行注释与空行被忽略', () => {
  const text = '\n# comment line\n# HELP m_x help text\n\n# TYPE m_x gauge\nm_x 1\n\n';
  const fams = parseMetrics(text);
  assert.equal(fams.length, 1);
  assert.equal(fams[0]!.samples.length, 1);
});

test('异常行容错：坏值/缺值/不闭合标签直接跳过', () => {
  const text = [
    'good_metric 5',
    'bad_value_metric abc',
    'no_value_metric',
    '{unclosed 3',
    'ok_metric{labels} 7',
    'also_ok 1e3',
  ].join('\n');
  const fams = parseMetrics(text);
  const names = fams.map((f) => f.name).sort();
  assert.deepEqual(names, ['also_ok', 'good_metric', 'ok_metric']);
  assert.equal(sumMetric(fams, 'also_ok'), 1000);
});

test('空输入返回空数组', () => {
  assert.deepEqual(parseMetrics(''), []);
  assert.deepEqual(parseMetrics('   \n \n'), []);
});

test('sumMetric 聚合与标签过滤', () => {
  const text = 'm{a="1"} 2\nm{a="2"} 3\nm{a="1"} 5\n';
  const fams = parseMetrics(text);
  assert.equal(sumMetric(fams, 'm'), 10);
  assert.equal(sumMetric(fams, 'm', { a: '1' }), 7);
  assert.equal(sumMetric(fams, 'missing'), null);
});

test('maxMetric / firstMetric（firstMetric 默认 sum 模式）', () => {
  const text = 'm{a="1"} 4\nm{a="2"} 9\nother 1\n';
  const fams = parseMetrics(text);
  assert.equal(maxMetric(fams, 'm'), 9);
  assert.deepEqual(firstMetric(fams, ['nope', 'm']), { name: 'm', value: 13 });
  assert.equal(firstMetric(fams, ['nope']), null);
  assert.deepEqual(firstMetric(fams, ['m'], 'max'), { name: 'm', value: 9 });
});

test('httpErrorRate：5xx 比例与空数据 null', () => {
  const text = [
    'ufot_http_requests_total{status="200"} 90',
    'ufot_http_requests_total{status="500"} 10',
  ].join('\n');
  const fams = parseMetrics(text);
  assert.equal(httpErrorRate(fams), 0.1);
  assert.equal(httpErrorRate(parseMetrics('# TYPE x gauge\nx 1\n')), null);
});

test('真实 /metrics 输出样例（ufot_admin_* 家族）', () => {
  const text = [
    '# HELP ufot_admin_build_info Admin API build information.',
    '# TYPE ufot_admin_build_info gauge',
    'ufot_admin_build_info{version="dev"} 1',
    '# TYPE go_goroutines gauge',
    'go_goroutines 27',
    '# TYPE ufot_admin_audit_append_failures_total counter',
    'ufot_admin_audit_append_failures_total 0',
    '# TYPE ufot_admin_bridge_circuit_open gauge',
    'ufot_admin_bridge_circuit_open 0',
  ].join('\n');
  const fams = parseMetrics(text);
  assert.equal(sumMetric(fams, 'go_goroutines'), 27);
  assert.equal(sumMetric(fams, 'ufot_admin_audit_append_failures_total'), 0);
  assert.equal(maxMetric(fams, 'ufot_admin_bridge_circuit_open'), 0);
});
