import type { ReactNode } from 'react';

interface BrandMarkProps {
  size?: number;
  className?: string;
}

/**
 * UFOT 字标：几何 UFO 环 + U 字负形，黑白单色。
 * 小尺寸（24px）下仍可辨识：笔画粗、对比强、无细碎细节。
 */
export function BrandMark({ size = 30, className }: BrandMarkProps): ReactNode {
  const id = `ufot-g-${size}`;
  return (
    <svg
      className={className}
      width={size}
      height={size}
      viewBox="0 0 32 32"
      role="img"
      aria-label="UFOT"
      style={{ display: 'block', flex: '0 0 auto' }}
    >
      <defs>
        <mask id={id}>
          <rect width="32" height="32" fill="#fff" />
          {/* U 字负形：从环体向下切开 */}
          <path d="M11 8 v8 a5 5 0 0 0 10 0 V8 h-3.4 v8 a1.6 1.6 0 0 1 -3.2 0 V8 Z" fill="#000" />
        </mask>
      </defs>
      {/* 外环碟形：椭圆盘 + 底部弧线，几何化的 UFO */}
      <g mask={`url(#${id})`}>
        <ellipse cx="16" cy="13" rx="14" ry="7.5" fill="currentColor" />
        <path d="M9.5 21.5 a9 4.2 0 0 0 13 0 l1.8 2.2 a11.5 5.6 0 0 1 -16.6 0 Z" fill="currentColor" />
      </g>
    </svg>
  );
}
