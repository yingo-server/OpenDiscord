import { Fragment, useEffect, useRef, type ReactNode } from 'react';
import type { SortOrder } from '../../api/types';
import { cn, formatNumber } from '../../lib/format';
import { IconChevronDown, IconChevronLeft, IconChevronRight, IconChevronUp, IconSearch } from './icons';
import { t } from '../i18n';

export interface Column<T> {
  key: string;
  header: ReactNode;
  /** 有值时列头可排序 */
  sortKey?: string;
  width?: number | string;
  align?: 'left' | 'right' | 'center';
  render: (row: T, index: number) => ReactNode;
}

export interface PaginationState {
  page: number;
  perPage: number;
  total: number;
  onPageChange: (page: number) => void;
  onPerPageChange?: (perPage: number) => void;
  perPageOptions?: number[];
}

export interface DataTableProps<T> {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  loading?: boolean;
  error?: string | null;
  emptyText?: string;
  selectable?: boolean;
  selected?: Set<string>;
  onSelectedChange?: (next: Set<string>) => void;
  /** 跨页全选提示（§9.1："已选本页 20 条 [选择全部 342 条]"） */
  allAcrossPages?: boolean;
  onSelectAllAcrossPages?: () => void;
  sort?: { key: string; order: SortOrder } | null;
  onSortChange?: (next: { key: string; order: SortOrder }) => void;
  pagination?: PaginationState;
  toolbar?: ReactNode;
  batchBar?: ReactNode;
  footerLeft?: ReactNode;
}

function pageWindow(current: number, totalPages: number): Array<number | 'gap'> {
  if (totalPages <= 7) return Array.from({ length: totalPages }, (_, i) => i + 1);
  const pages = new Set<number>([1, totalPages, current, current - 1, current + 1]);
  const list = [...pages].filter((p) => p >= 1 && p <= totalPages).sort((a, b) => a - b);
  const out: Array<number | 'gap'> = [];
  list.forEach((p, i) => {
    if (i > 0 && p - (list[i - 1] ?? p) > 1) out.push('gap');
    out.push(p);
  });
  return out;
}

/**
 * 通用数据表格（docs/web-ui-spec.md §9.1）：
 * 行高 48px、无斑马纹、hover 底色、排序图标、跨页批量、底部分页右对齐。
 * loading 时置灰但不清空（筛选切换不闪空表）。
 */
export function DataTable<T>({
  columns,
  rows,
  rowKey,
  loading = false,
  error = null,
  emptyText = '没有匹配的数据',
  selectable = false,
  selected,
  onSelectedChange,
  allAcrossPages = false,
  onSelectAllAcrossPages,
  sort,
  onSortChange,
  pagination,
  toolbar,
  batchBar,
  footerLeft,
}: DataTableProps<T>) {
  const headerRef = useRef<HTMLInputElement>(null);
  const selection = selected ?? new Set<string>();
  const pageIds = rows.map(rowKey);
  const selectedOnPage = pageIds.filter((id) => selection.has(id)).length;
  const allOnPage = rows.length > 0 && selectedOnPage === rows.length;

  useEffect(() => {
    if (headerRef.current) {
      headerRef.current.indeterminate = selectedOnPage > 0 && !allOnPage;
    }
  }, [selectedOnPage, allOnPage]);

  const toggleRow = (id: string) => {
    const next = new Set(selection);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    onSelectedChange?.(next);
  };

  const togglePage = () => {
    const next = new Set(selection);
    if (allOnPage) pageIds.forEach((id) => next.delete(id));
    else pageIds.forEach((id) => next.add(id));
    onSelectedChange?.(next);
  };

  const handleSort = (key: string) => {
    if (!onSortChange) return;
    if (sort?.key === key) {
      onSortChange({ key, order: sort.order === 'asc' ? 'desc' : 'asc' });
    } else {
      onSortChange({ key, order: 'asc' });
    }
  };

  const totalPages = pagination ? Math.max(1, Math.ceil(pagination.total / pagination.perPage)) : 1;

  return (
    <div className="card overflow-hidden">
      {toolbar && <div className="border-b border-border-default px-4 py-3">{toolbar}</div>}

      {error && (
        <div className="border-b border-danger-300 bg-danger-50 px-4 py-2 text-body-sm text-danger-700">{error}</div>
      )}

      <div className={cn('transition-opacity duration-normal', loading && 'pointer-events-none opacity-50')}>
        <div className="overflow-x-auto">
          <table className="tbl">
            <thead>
              <tr>
                {selectable && (
                  <th style={{ width: 44 }} className="text-center">
                    <input
                      ref={headerRef}
                      type="checkbox"
                      className="h-4 w-4 accent-primary-500 align-middle"
                      checked={allOnPage}
                      onChange={togglePage}
                      aria-label={t('dataTable.selectAllPage')}
                    />
                  </th>
                )}
                {columns.map((col) => {
                  const active = sort && col.sortKey && sort.key === col.sortKey;
                  return (
                    <th
                      key={col.key}
                      style={col.width ? { width: col.width } : undefined}
                      className={cn(
                        col.align === 'right' && 'text-right',
                        col.align === 'center' && 'text-center',
                        col.sortKey && 'tbl-sortable',
                      )}
                      onClick={col.sortKey ? () => handleSort(col.sortKey!) : undefined}
                      aria-sort={active ? (sort!.order === 'asc' ? 'ascending' : 'descending') : undefined}
                    >
                      <span className="inline-flex items-center gap-1">
                        {col.header}
                        {col.sortKey && (
                          <span className={cn('text-text-tertiary', active && 'text-primary-500')}>
                            {active && sort!.order === 'desc' ? <IconChevronDown size={14} /> : <IconChevronUp size={14} />}
                          </span>
                        )}
                      </span>
                    </th>
                  );
                })}
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 && !loading && (
                <tr>
                  <td colSpan={columns.length + (selectable ? 1 : 0)}>
                    <div className="flex flex-col items-center gap-2 py-12 text-center">
                      <IconSearch size={24} className="text-text-tertiary" />
                      <p className="text-body-sm text-text-tertiary">{emptyText}</p>
                    </div>
                  </td>
                </tr>
              )}
              {rows.map((row, index) => {
                const id = rowKey(row);
                const isSelected = selection.has(id);
                return (
                  <tr key={id} data-selected={isSelected}>
                    {selectable && (
                      <td className="text-center">
                        <input
                          type="checkbox"
                          className="h-4 w-4 accent-primary-500 align-middle"
                          checked={isSelected}
                          onChange={() => toggleRow(id)}
                          aria-label={`选择 ${id}`}
                        />
                      </td>
                    )}
                    {columns.map((col) => (
                      <td
                        key={col.key}
                        className={cn(
                          col.align === 'right' && 'text-right',
                          col.align === 'center' && 'text-center',
                        )}
                      >
                        {col.render(row, index)}
                      </td>
                    ))}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>

        {selectable && selectedOnPage > 0 && (
          <div className="flex flex-wrap items-center gap-3 border-t border-border-default bg-bg-surface-2 px-4 py-2">
            <span className="text-body-sm text-text-secondary">
              {t('dataTable.selectedPage')} <span className="nums font-semibold">{selectedOnPage}</span> 条
              {selection.size > selectedOnPage && (
                <span className="nums"> （共 {formatNumber(selection.size)} 条）</span>
              )}
            </span>
            {onSelectAllAcrossPages && pagination && pagination.total > selectedOnPage && !allAcrossPages && (
              <button type="button" className="text-body-sm text-primary-600 underline" onClick={onSelectAllAcrossPages}>
                选择全部 {formatNumber(pagination.total)} 条
              </button>
            )}
            {allAcrossPages && (
              <span className="tag bg-primary-50 text-primary-700">已跨页全选 {formatNumber(selection.size)} 条</span>
            )}
            <div className="ml-auto flex items-center gap-2">{batchBar}</div>
          </div>
        )}
      </div>

      {pagination && (
        <div className="flex flex-wrap items-center justify-end gap-3 border-t border-border-default px-4 py-2">
          {footerLeft && <div className="mr-auto text-body-sm text-text-tertiary">{footerLeft}</div>}
          <span className="nums text-body-sm text-text-secondary">共 {formatNumber(pagination.total)} 条</span>
          <div className="flex items-center gap-1">
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              disabled={pagination.page <= 1}
              onClick={() => pagination.onPageChange(pagination.page - 1)}
              aria-label={t('dataTable.prevPage')}
            >
              <IconChevronLeft size={14} />
            </button>
            {pageWindow(pagination.page, totalPages).map((p, i) =>
              p === 'gap' ? (
                <span key={`gap-${i}`} className="px-1 text-text-tertiary">
                  …
                </span>
              ) : (
                <button
                  key={p}
                  type="button"
                  className={cn(
                    'btn btn-sm nums min-w-8',
                    p === pagination.page ? 'btn-primary' : 'btn-secondary',
                  )}
                  aria-current={p === pagination.page ? 'page' : undefined}
                  onClick={() => pagination.onPageChange(p)}
                >
                  {p}
                </button>
              ),
            )}
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              disabled={pagination.page >= totalPages}
              onClick={() => pagination.onPageChange(pagination.page + 1)}
              aria-label={t('dataTable.nextPage')}
            >
              <IconChevronRight size={14} />
            </button>
          </div>
          {pagination.onPerPageChange && (
            <label className="flex items-center gap-2 text-body-sm text-text-secondary">
              每页
              <select
                className="select w-auto"
                style={{ height: 28, paddingRight: 28 }}
                value={pagination.perPage}
                onChange={(e) => pagination.onPerPageChange?.(Number(e.target.value))}
              >
                {(pagination.perPageOptions ?? [20, 50, 100]).map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      )}
    </div>
  );
}

/** 筛选条：与表格同宽，位于表格上方（§9.1） */
export function FilterBar({ children, onReset }: { children: ReactNode; onReset?: () => void }) {
  return (
    <div className="mb-4 flex flex-wrap items-end gap-3">
      {children}
      {onReset && (
        <button type="button" className="btn btn-ghost btn-sm mb-0.5" onClick={onReset}>
          重置
        </button>
      )}
    </div>
  );
}

/** 搜索输入：300ms 防抖由调用方用 useDebouncedValue 实现 */
export function SearchInput({
  value,
  onChange,
  placeholder = t('dataTable.search'),
  width = 280,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  width?: number;
}) {
  return (
    <div className="relative" style={{ width }}>
      <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-text-tertiary">
        <IconSearch size={16} />
      </span>
      <input
        className="input pl-9"
        value={value}
        placeholder={placeholder}
        onChange={(event) => onChange(event.target.value)}
        aria-label={placeholder}
      />
    </div>
  );
}

export const TableRowFragment = Fragment;
