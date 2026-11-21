import { useEffect, useState } from 'react';
import { useAuth } from '../../auth';
import { cn } from '../../lib/format';
import { playSound, setSoundEnabled, soundEnabled, unlockAudio } from '../../lib/sound';
import { IconBell, IconBellOff, IconLogout, IconMenu, IconMoon, IconSun } from '../components/icons';
import { StatusTag } from '../components/StatusTag';
import type { NavItem } from './nav';

const ROLE_LABEL: Record<string, string> = {
  superadmin: '超级管理员',
  admin: '管理员',
  moderator: '审核员',
  auditor: '审计员',
};

function useThemeController() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains('dark'));

  const toggle = () => {
    const next = !dark;
    const root = document.documentElement;
    root.classList.add('theme-transitioning');
    root.classList.toggle('dark', next);
    root.dataset.theme = next ? 'dark' : 'light';
    try {
      localStorage.setItem('oc-theme', next ? 'dark' : 'light');
    } catch {
      /* 隐私模式忽略 */
    }
    window.setTimeout(() => root.classList.remove('theme-transitioning'), 300);
    setDark(next);
  };

  return { dark, toggle };
}

/** 顶栏 56px：面包屑 + 管理员身份 + 退出（§9） */
export function TopBar({ item, onOpenNav }: { item: NavItem; onOpenNav: () => void }) {
  const { admin, logout } = useAuth();
  const { dark, toggle } = useThemeController();
  const [busy, setBusy] = useState(false);
  const [sound, setSound] = useState(soundEnabled());

  const toggleSound = () => {
    const next = !sound;
    setSoundEnabled(next);
    setSound(next);
    unlockAudio();
    if (next) playSound('success');
  };

  useEffect(() => {
    document.title = `${item.label} · UFOT 管理后台`;
  }, [item.label]);

  return (
    <header className="flex h-top-bar shrink-0 items-center gap-3 border-b border-border-default bg-bg-surface px-4 lg:px-6">
      <button type="button" className="btn btn-ghost btn-sm lg:hidden" onClick={onOpenNav} aria-label="打开导航">
        <IconMenu size={18} />
      </button>

      <nav aria-label="面包屑" className="min-w-0 flex-1">
        <ol className="flex items-center gap-2 text-table text-text-secondary">
          <li className="hidden text-text-tertiary sm:block">{item.group === item.label ? '概览' : '控制台'}</li>
          <li className="hidden text-text-tertiary sm:block" aria-hidden>
            /
          </li>
          <li className="truncate font-semibold text-text-primary">{item.label}</li>
          <li className="hidden truncate text-body-sm text-text-tertiary xl:block">— {item.description}</li>
        </ol>
      </nav>

      <button
        type="button"
        className="btn btn-ghost btn-sm"
        onClick={toggle}
        aria-label={dark ? '切换到亮色主题' : '切换到暗色主题'}
        title={dark ? '切换到亮色主题' : '切换到暗色主题'}
      >
        {dark ? <IconSun size={16} /> : <IconMoon size={16} />}
      </button>

      <button
        type="button"
        className={cn('btn btn-ghost btn-sm', sound && 'text-primary-600')}
        onClick={toggleSound}
        aria-label={sound ? '关闭提示音' : '开启提示音'}
        aria-pressed={sound}
        title={sound ? '提示音已开启（点击关闭）' : '提示音已关闭（点击开启）'}
      >
        {sound ? <IconBell size={16} /> : <IconBellOff size={16} />}
      </button>

      <div className="hidden items-center gap-2 border-l border-border-default pl-3 sm:flex">
        <div className="text-right">
          <div className="text-body-sm font-semibold leading-5 text-text-primary">{admin?.display_name ?? '未登录'}</div>
          <div className="text-caption leading-4 text-text-tertiary">@{admin?.username ?? '-'}</div>
        </div>
        {admin && (
          <StatusTag tone={admin.role === 'superadmin' ? 'primary' : 'muted'}>
            {ROLE_LABEL[admin.role] ?? admin.role}
          </StatusTag>
        )}
        {admin && !admin.totp_enabled && <StatusTag tone="danger">未开 TOTP</StatusTag>}
      </div>

      <button
        type="button"
        className={cn('btn btn-secondary btn-sm', busy && 'opacity-60')}
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          try {
            await logout();
          } finally {
            setBusy(false);
          }
        }}
      >
        <IconLogout size={14} />
        <span className="hidden sm:inline">退出</span>
      </button>
    </header>
  );
}
