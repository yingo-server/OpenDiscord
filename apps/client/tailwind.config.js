/**
 * UFOT Fluent 设计令牌（WinUI 3 / Fluent Design）
 * 所有颜色引用 CSS 变量（RGB 三元组），亮/暗主题在 styles/index.css 中切换；
 * 保留全部旧令牌名（primary / bg / text / line / ink），值改为 Fluent 调色板——
 * 组件零改动即可换肤；新增 Fluent 专用别名（acrylic/mica/focus/fluent 阴影等）。
 * @type {import('tailwindcss').Config}
 */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    screens: {
      // 沿用阶段 1-2 的语义断点（tablet/desktop），补回 Tailwind 默认 xl/2xl 前缀；
      // sm 保留给消息转发面板的移动端断点（<640px 贴底），避免与默认 sm 冲突。
      sm: '640px',
      xl: '1280px',
      '2xl': '1536px',
      tablet: '768px',
      desktop: '1200px',
    },
    extend: {
      colors: {
        primary: {
          50: 'rgb(var(--primary-50) / <alpha-value>)',
          100: 'rgb(var(--primary-100) / <alpha-value>)',
          300: 'rgb(var(--primary-300) / <alpha-value>)',
          500: 'rgb(var(--primary-500) / <alpha-value>)',
          600: 'rgb(var(--primary-600) / <alpha-value>)',
          700: 'rgb(var(--primary-700) / <alpha-value>)',
          900: 'rgb(var(--primary-900) / <alpha-value>)',
        },
        success: {
          50: 'rgb(var(--success-50) / <alpha-value>)',
          100: 'rgb(var(--success-100) / <alpha-value>)',
          300: 'rgb(var(--success-300) / <alpha-value>)',
          500: 'rgb(var(--success-500) / <alpha-value>)',
          600: 'rgb(var(--success-600) / <alpha-value>)',
          700: 'rgb(var(--success-700) / <alpha-value>)',
          900: 'rgb(var(--success-900) / <alpha-value>)',
        },
        warning: {
          50: 'rgb(var(--warning-50) / <alpha-value>)',
          100: 'rgb(var(--warning-100) / <alpha-value>)',
          300: 'rgb(var(--warning-300) / <alpha-value>)',
          500: 'rgb(var(--warning-500) / <alpha-value>)',
          600: 'rgb(var(--warning-600) / <alpha-value>)',
          700: 'rgb(var(--warning-700) / <alpha-value>)',
          900: 'rgb(var(--warning-900) / <alpha-value>)',
        },
        danger: {
          50: 'rgb(var(--danger-50) / <alpha-value>)',
          100: 'rgb(var(--danger-100) / <alpha-value>)',
          300: 'rgb(var(--danger-300) / <alpha-value>)',
          500: 'rgb(var(--danger-500) / <alpha-value>)',
          600: 'rgb(var(--danger-600) / <alpha-value>)',
          700: 'rgb(var(--danger-700) / <alpha-value>)',
          900: 'rgb(var(--danger-900) / <alpha-value>)',
        },
        info: {
          50: 'rgb(var(--info-50) / <alpha-value>)',
          100: 'rgb(var(--info-100) / <alpha-value>)',
          300: 'rgb(var(--info-300) / <alpha-value>)',
          500: 'rgb(var(--info-500) / <alpha-value>)',
          600: 'rgb(var(--info-600) / <alpha-value>)',
          700: 'rgb(var(--info-700) / <alpha-value>)',
          900: 'rgb(var(--info-900) / <alpha-value>)',
        },

        // 表面 / 文本 / 边框（语义层，组件只引用这些）
        page: 'rgb(var(--bg-page) / <alpha-value>)',
        surface: {
          DEFAULT: 'rgb(var(--bg-surface) / <alpha-value>)',
          2: 'rgb(var(--bg-surface-2) / <alpha-value>)',
          elevated: 'rgb(var(--bg-elevated) / <alpha-value>)',
          mica: 'rgb(var(--mica-base) / <alpha-value>)',
        },
        ink: {
          DEFAULT: 'rgb(var(--text-primary) / <alpha-value>)',
          secondary: 'rgb(var(--text-secondary) / <alpha-value>)',
          tertiary: 'rgb(var(--text-tertiary) / <alpha-value>)',
          inverse: 'rgb(var(--text-inverse) / <alpha-value>)',
        },
        line: {
          DEFAULT: 'rgb(var(--border-default) / <alpha-value>)',
          input: 'rgb(var(--border-input) / <alpha-value>)',
          acrylic: 'rgb(var(--acrylic-stroke) / <alpha-value>)',
        },
        focus: 'rgb(var(--focus-ring) / <alpha-value>)',
        scrim: 'rgb(var(--scrim) / <alpha-value>)',
      },

      // Fluent 字号：Segoe UI Variable 的 14/15/17/20/24/28 阶梯
      fontSize: {
        display: ['28px', { lineHeight: '36px', fontWeight: '600' }],
        title: ['20px', { lineHeight: '28px', fontWeight: '600' }],
        body: ['15px', { lineHeight: '22px', fontWeight: '400' }],
        'body-sm': ['13px', { lineHeight: '20px', fontWeight: '400' }],
        caption: ['12px', { lineHeight: '16px', fontWeight: '400' }],
        table: ['14px', { lineHeight: '20px', fontWeight: '400' }],
      },
      fontFamily: {
        sans: [
          'Segoe UI Variable Text',
          'Segoe UI Variable',
          'Segoe UI',
          'system-ui',
          '-apple-system',
          'PingFang SC',
          'Microsoft YaHei',
          'Noto Sans SC',
          'sans-serif',
        ],
        // Fluent 标题/品牌字标：Variable Display 字重轴
        display: [
          'Segoe UI Variable Display',
          'Segoe UI Variable',
          'Segoe UI',
          'system-ui',
          'PingFang SC',
          'Microsoft YaHei',
          'sans-serif',
        ],
        nums: [
          'Cascadia Mono',
          'SF Mono',
          'ui-monospace',
          'Consolas',
          'Noto Sans SC',
          'monospace',
        ],
      },

      // 4px 网格（尺寸令牌沿用，保证组件无改动）
      spacing: {
        0.5: '2px',
        1: '4px',
        2: '8px',
        3: '12px',
        4: '16px',
        6: '24px',
        8: '32px',
        12: '48px',
        16: '64px',
        'control-sm': '28px',
        'control-md': '36px',
        'control-lg': '44px',
        'avatar-xs': '24px',
        'avatar-sm': '32px',
        'avatar-md': '48px',
        'avatar-lg': '64px',
        'avatar-xl': '96px',
        'row-conv': '64px',
        'row-conv-mobile': '72px',
        'row-table': '48px',
        sidebar: '280px',
        detail: '320px',
      },

      // Fluent 圆角：控件 4/6/8，卡片 8/12，胶囊 999
      borderRadius: {
        sm: '4px',
        md: '8px',
        lg: '12px',
        tail: '2px',
        xl: '16px',
        full: '999px',
      },

      // Fluent 层级阴影：低对比、大扩散（暗色下由令牌变量加深）
      boxShadow: {
        sm: '0 1px 2px rgb(0 0 0 / 0.06)',
        md: 'var(--shadow-flyout)',
        lg: 'var(--shadow-dialog)',
        card: 'var(--shadow-card)',
      },

      // Fluent 动效：ease-out 进入、ease-in-out 位移
      transitionDuration: {
        micro: '100ms',
        normal: '200ms',
        slow: '300ms',
        skeleton: '1200ms',
      },
      transitionTimingFunction: {
        out: 'cubic-bezier(0, 0, 0.2, 1)',
        'in-out': 'cubic-bezier(0.4, 0, 0.2, 1)',
        in: 'ease-in',
      },

      zIndex: {
        base: '0',
        sticky: '10',
        'network-bar': '20',
        drawer: '30',
        modal: '40',
        menu: '50',
        toast: '60',
        tooltip: '70',
      },

      keyframes: {
        shimmer: {
          '0%': { backgroundPosition: '200% 0' },
          '100%': { backgroundPosition: '-200% 0' },
        },
        'fade-in': {
          from: { opacity: '0' },
          to: { opacity: '1' },
        },
        'slide-up': {
          from: { opacity: '0', transform: 'translateY(8px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
        'scale-in': {
          from: { opacity: '0', transform: 'scale(0.96)' },
          to: { opacity: '1', transform: 'scale(1)' },
        },
        'pulse-dot': {
          '0%, 100%': { opacity: '1' },
          '50%': { opacity: '0.35' },
        },
        'wave-breath': {
          '0%, 100%': { transform: 'scaleY(1)' },
          '50%': { transform: 'scaleY(1.5)' },
        },
      },
      animation: {
        shimmer: 'shimmer 1.2s linear infinite',
        'fade-in': 'fade-in 200ms cubic-bezier(0, 0, 0.2, 1)',
        'slide-up': 'slide-up 200ms cubic-bezier(0, 0, 0.2, 1)',
        'scale-in': 'scale-in 200ms cubic-bezier(0, 0, 0.2, 1)',
        'pulse-dot': 'pulse-dot 1.5s ease-in-out infinite',
        'wave-breath': 'wave-breath 100ms ease-in-out infinite',
      },
    },
  },
  plugins: [],
};
