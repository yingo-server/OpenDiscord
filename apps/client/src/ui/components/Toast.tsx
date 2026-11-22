import { useEffect, useState, type ReactNode } from 'react';

export function toast(message: string): void {
  window.dispatchEvent(new CustomEvent('ufot:toast', { detail: { message } }));
}

export function ToastHost(): ReactNode {
  const [items, setItems] = useState<Array<{ id: number; message: string }>>([]);
  useEffect(() => {
    let next = 0;
    const handler = (event: Event) => {
      const message = (event as CustomEvent<{ message: string }>).detail?.message;
      if (!message) return;
      const id = ++next;
      setItems((current) => [...current.slice(-2), { id, message }]);
      window.setTimeout(() => setItems((current) => current.filter((item) => item.id !== id)), 3200);
    };
    window.addEventListener('ufot:toast', handler);
    window.addEventListener('ufot:notice', handler);
    return () => {
      window.removeEventListener('ufot:toast', handler);
      window.removeEventListener('ufot:notice', handler);
    };
  }, []);
  return <div className="toast-wrap" role="status" aria-live="polite">{items.map((item) => <div className="toast" key={item.id}>{item.message}</div>)}</div>;
}
