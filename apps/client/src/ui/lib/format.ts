export function cn(...classes: Array<string | false | null | undefined>): string {
  return classes.filter(Boolean).join(' ');
}

export function initials(name: string): string {
  const value = name.trim();
  if (!value) return '?';
  const parts = value.split(/\s+/);
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
  return value.slice(0, 2).toUpperCase();
}

export function formatTime(ts: number): string {
  if (!ts) return '';
  return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

export function formatListTime(ts: number): string {
  if (!ts) return '';
  const date = new Date(ts);
  if (date.toDateString() === new Date().toDateString()) return formatTime(ts);
  return `${date.getMonth() + 1}/${date.getDate()}`;
}

export function safeUrl(url: string): string | null {
  return /^https?:\/\//i.test(url) ? url : null;
}
