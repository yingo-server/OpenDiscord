import { cn } from '../../lib/format';
import { IconChevronLeft, IconChevronRight } from '../components/icons';
import { NAV_GROUPS } from './nav';

/**
 * 左侧固定侧边导航：展开 240px / 收窄 64px 图标态（docs/web-ui-spec.md §9）。
 * 分组标题在收窄态隐藏，靠 title 提供可访问名称。
 */
export function SideNav({
  current,
  collapsed,
  onNavigate,
  onToggleCollapsed,
}: {
  current: string;
  collapsed: boolean;
  onNavigate: (path: string) => void;
  onToggleCollapsed: () => void;
}) {
  return (
    <div className="flex h-full flex-col border-r border-border-default bg-bg-surface">
      <div
        className={cn(
          'flex h-top-bar shrink-0 items-center gap-2 border-b border-border-default',
          collapsed ? 'justify-center px-2' : 'px-4',
        )}
      >
        <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-primary-500 text-body-sm font-bold text-text-inverse">
          OC
        </span>
        {!collapsed && (
          <div className="min-w-0">
            <div className="truncate text-table font-semibold text-text-primary">UFOT</div>
            <div className="text-caption text-text-tertiary">管理后台</div>
          </div>
        )}
      </div>

      <nav className="flex-1 space-y-4 overflow-y-auto px-2 py-3" aria-label="主导航">
        {NAV_GROUPS.map((group) => (
          <div key={group.title}>
            {!collapsed && (
              <div className="px-3 pb-1 text-caption font-semibold uppercase tracking-wide text-text-tertiary">
                {group.title}
              </div>
            )}
            <ul className="space-y-0.5">
              {group.items.map((item) => {
                const active = current === item.path;
                const Icon = item.icon;
                return (
                  <li key={item.path}>
                    <a
                      href={`#${item.path}`}
                      className={cn('nav-item', collapsed && 'justify-center px-0')}
                      aria-current={active ? 'page' : undefined}
                      title={collapsed ? item.label : item.description}
                      onClick={(event) => {
                        event.preventDefault();
                        onNavigate(item.path);
                      }}
                    >
                      <Icon size={18} className={active ? 'text-primary-500' : 'text-text-tertiary'} />
                      {!collapsed && <span className="truncate">{item.label}</span>}
                    </a>
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
      </nav>

      <div className="shrink-0 border-t border-border-default p-2">
        <button
          type="button"
          className={cn('btn btn-ghost btn-sm w-full', collapsed ? 'justify-center px-0' : 'justify-start')}
          onClick={onToggleCollapsed}
          aria-label={collapsed ? '展开侧边导航' : '收窄侧边导航'}
          title={collapsed ? '展开侧边导航' : '收窄侧边导航'}
        >
          {collapsed ? <IconChevronRight size={16} /> : <IconChevronLeft size={16} />}
          {!collapsed && <span>收窄</span>}
        </button>
      </div>
    </div>
  );
}
