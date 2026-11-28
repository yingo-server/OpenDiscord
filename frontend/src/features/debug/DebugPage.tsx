/** /debug：读取 IndexedDB 中的全部请求日志，纯文本展示，便于复制。 */

import { useEffect, useState } from 'react';
import { Button } from '@/ui/primitives/controls';
import { readDebug, clearDebug, debugText, type DebugEntry } from '@/core/debug/db';

export function DebugPage() {
  const [entries, setEntries] = useState<DebugEntry[]>([]);
  const [loading, setLoading] = useState(true);

  const load = async () => {
    setLoading(true);
    setEntries(await readDebug());
    setLoading(false);
  };

  useEffect(() => { void load(); }, []);

  return (
    <div className="uf-page">
      <header className="uf-topbar">
        <span className="uf-topbar__title">调试日志</span>
        <div className="uf-grow" />
        <Button small icon="refresh" onClick={() => void load()}>刷新</Button>
        <Button small variant="danger" onClick={() => void clearDebug().then(load)}>清空</Button>
      </header>
      <div className="uf-page__body">
        <p className="uf-faint">共 {entries.length} 条。所有 HTTP / WS 请求详情均已写入 IndexedDB（令牌已脱敏）。</p>
        <pre className="uf-pre">{loading ? '读取中…' : debugText(entries) || '（暂无日志）'}</pre>
      </div>
    </div>
  );
}
