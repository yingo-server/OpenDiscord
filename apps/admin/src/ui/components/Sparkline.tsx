/** 纯 SVG sparkline（无图表库）：折线 + 最新点，ViewBox 固定，自适应宽度。 */
import type { Sample } from '../../lib/polling';
import { t } from '../i18n';

export function Sparkline({
  points,
  width = 240,
  height = 44,
  stroke = 'rgb(var(--oc-primary-500))',
  ariaLabel,
}: {
  points: Sample[];
  width?: number;
  height?: number;
  stroke?: string;
  ariaLabel?: string;
}) {
  if (points.length < 2) {
    return (
      <div
        className="flex items-center justify-center text-caption text-text-tertiary"
        style={{ height }}
        aria-label={ariaLabel}
      >
        采样中…（每 5s 一个点，最多 60 个）
      </div>
    );
  }
  const values = points.map((p) => p.value);
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const pad = 3;
  const w = width - pad * 2;
  const h = height - pad * 2;
  const coords = points.map((p, i) => {
    const x = pad + (i / (points.length - 1)) * w;
    const y = pad + h - ((p.value - min) / span) * h;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });
  const last = points[points.length - 1]!;
  const lastX = pad + w;
  const lastY = pad + h - ((last.value - min) / span) * h;

  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      width="100%"
      height={height}
      role="img"
      aria-label={ariaLabel ?? t('common.trend')}
      preserveAspectRatio="none"
    >
      <polyline points={coords.join(' ')} fill="none" stroke={stroke} strokeWidth="1.5" strokeLinejoin="round" />
      <circle cx={lastX} cy={lastY} r="2.5" fill={stroke} />
    </svg>
  );
}
