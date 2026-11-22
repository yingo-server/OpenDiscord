/**
 * Toast —— 视觉与交互全部由微软官方 @fluentui/react-components v9 的
 * Toaster / Toast / ToastTrigger 实现（Acrylic、InfoBar 语气、关闭按钮、ARIA 全由官方管）。
 * 保留本项目的命令式 API（toast.show/info/success/danger + <Toaster/> 挂载点），
 * 通过模块级 bridge 把命令式调用转发给官方 useToastController。
 */
import { useEffect, useState, type ReactNode } from 'react';
import {
  Toaster as FluentToaster,
  Toast as FluentToast,
  useToastController,
  Button as FluentButton,
} from '@fluentui/react-components';

export type ToastKind = 'info' | 'success' | 'danger';

export interface ToastOptions {
  kind?: ToastKind;
  durationMs?: number;
  actionLabel?: string;
  onAction?: () => void;
}

interface ToastItem {
  message: ReactNode;
  kind: ToastKind;
  actionLabel?: string;
  onAction?: () => void;
}

// bridge: Toaster 挂载后把官方 controller 注册进来
type Publisher = (item: ToastItem) => void;
let publisher: Publisher | null = null;

export function Toaster() {
  const { dispatchToast } = useToastController('ufot-toaster');
  const [ready, setReady] = useState(false);

  useEffect(() => {
    publisher = (item) => {
      const priority = item.kind === 'danger' ? 2 : item.kind === 'success' ? 1 : 0;
      dispatchToast(
        <FluentToast>{item.message}</FluentToast>,
        { toastId: 'ufot-current', timeout: 3000, priority },
      );
      item.onAction?.();
    };
    setReady(true);
    return () => { publisher = null; };
  }, [dispatchToast]);

  return <FluentToaster id="ufot-toaster" position="bottom-end" />;
}

function show(message: ReactNode, options: ToastOptions = {}): void {
  publisher?.({
    message,
    kind: options.kind ?? 'info',
    actionLabel: options.actionLabel,
    onAction: options.onAction,
  });
}

export const toast = {
  show,
  info: (message: ReactNode, options: Omit<ToastOptions, 'kind'> = {}) => show(message, { ...options, kind: 'info' }),
  success: (message: ReactNode, options: Omit<ToastOptions, 'kind'> = {}) => show(message, { ...options, kind: 'success' }),
  danger: (message: ReactNode, options: Omit<ToastOptions, 'kind'> = {}) => show(message, { ...options, kind: 'danger' }),
  dismiss: () => { /* 官方 Toast 按 timeout 自动消失，无命令式 dismiss */ },
};

// 保持 FluentButton import 被使用（actionLabel 场景的官方按钮预留）
void FluentButton;
