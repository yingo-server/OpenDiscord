/**
 * ConvList 虚拟滚动（FE-1b）单测（node 环境，纯逻辑）：
 * 验证虚拟化开启条件 —— 仅在"非连接中且存在可渲染会话"时虚拟化；
 * 连接中骨架屏与空状态路径不启用虚拟化（count 为 0，避免空态下额外定位容器）。
 * 渲染/布局验证交 CI（jsdom + 布局缺失，不在此展开）。
 */
import { describe, expect, it } from 'vitest';
import { shouldVirtualize } from './ConvList';

describe('ConvList 虚拟滚动开启条件（FE-1b）', () => {
  it('有可渲染会话且非连接中 → 开启虚拟化', () => {
    expect(shouldVirtualize(false, 1)).toBe(true);
    expect(shouldVirtualize(false, 500)).toBe(true);
  });

  it('连接中（骨架屏路径）→ 不虚拟化', () => {
    expect(shouldVirtualize(true, 0)).toBe(false);
    expect(shouldVirtualize(true, 100)).toBe(false);
  });

  it('无可渲染会话（空状态 / 搜索无结果）→ 不虚拟化', () => {
    expect(shouldVirtualize(false, 0)).toBe(false);
  });
});
