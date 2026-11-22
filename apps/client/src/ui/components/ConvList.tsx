import { useMemo, useState, type ReactNode } from 'react';
import { conversations as getConversations, getAppServices } from '../../app/services';
import type { ChatState } from '../../state/chat';
import type { Conversation } from '../../domain/types';
import { t } from '../i18n';
import { cn, formatListTime, initials } from '../lib/format';
import { toast } from './Toast';

function ConvRow({ conv, active, onClick }: { conv: Conversation; active: boolean; onClick: () => void }): ReactNode {
  return (
    <button type="button" className={cn('conv-row', active && 'active')} onClick={onClick}>
      <span className="avatar">{initials(conv.title)}</span>
      <span style={{ minWidth: 0 }}>
        <span className="title">{conv.title}</span>
        <span className="conv-tags">
          {conv.pinned ? <span className="tag">{t('conv.pinned')}</span> : null}
          {conv.muted ? <span className="tag">{t('conv.muted')}</span> : null}
          <span className="tag">{conv.type === 'group' ? t('conv.group') : t('conv.direct')}</span>
        </span>
        <span className="preview">{conv.lastText}</span>
      </span>
      <span className="meta">
        <span>{formatListTime(conv.lastAt)}</span>
        {conv.unread > 0 ? <span className="unread">{conv.unread > 99 ? '99+' : conv.unread}</span> : null}
      </span>
    </button>
  );
}

function NewConvDialog({ onClose }: { onClose: () => void }): ReactNode {
  const [title, setTitle] = useState('');
  const [members, setMembers] = useState('');
  const services = getAppServices();
  const create = () => {
    const ids = members.split(/[,\s]+/).map((value) => Number(value.trim())).filter((n) => Number.isInteger(n) && n > 0);
    services.createConversation(ids.length > 1 ? 'group' : 'direct', ids, title.trim());
    onClose();
  };
  return (
    <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
      <div className="modal" role="dialog" aria-modal="true" aria-label={t('conv.create')}>
        <h2>{t('conv.create')}</h2>
        <p className="field hint">{t('conv.create.hint')}</p>
        <div className="field"><label htmlFor="conv-title">{t('conv.title')}</label><input id="conv-title" className="input" value={title} onChange={(event) => setTitle(event.target.value)} /></div>
        <div className="field"><label htmlFor="conv-members">{t('conv.members')}</label><input id="conv-members" className="input" value={members} onChange={(event) => setMembers(event.target.value)} placeholder="123, 456" /></div>
        <div className="modal-actions">
          <button className="btn" type="button" onClick={onClose}>{t('common.cancel')}</button>
          <button className="btn primary" type="button" onClick={create}>{t('conv.confirm')}</button>
        </div>
      </div>
    </div>
  );
}

export function ConvList({ state, onSelect, onOpenSettings }: { state: ChatState; onSelect: (id: number) => void; onOpenSettings: () => void }): ReactNode {
  const [query, setQuery] = useState('');
  const [showNew, setShowNew] = useState(false);
  const list = useMemo(() => getConversations().filter((conv) => conv.title.toLowerCase().includes(query.toLowerCase())), [query, state.conversations]);
  return (
    <aside className={cn('conv-pane', state.activeConvId !== null && 'hidden-mobile')}>
      <div className="pane-head">
        <h1 className="pane-title">{t('nav.chats')}</h1>
        <div className="pane-actions">
          <button className="icon-btn primary" type="button" aria-label={t('nav.new.chat')} onClick={() => setShowNew(true)}>+</button>
          <button className="icon-btn" type="button" aria-label={t('nav.settings')} onClick={onOpenSettings}>⚙</button>
        </div>
      </div>
      <div className="search-wrap">
        <input className="input" type="search" placeholder={t('nav.search')} value={query} onChange={(event) => setQuery(event.target.value)} />
      </div>
      <div className="conv-list">
        {!state.conversations.length ? (
          <div className="empty" style={{ minHeight: 200 }}><div><h3>{t('conv.empty')}</h3><p>{t('conv.empty.hint')}</p></div></div>
        ) : list.length ? list.map((conv) => (
          <ConvRow key={conv.id} conv={conv} active={state.activeConvId === conv.id} onClick={() => onSelect(conv.id)} />
        )) : <div className="pane-sub">{t('common.empty')}</div>}
      </div>
      {showNew ? <NewConvDialog onClose={() => setShowNew(false)} /> : null}
      <button type="button" hidden onClick={() => toast(t('common.retry'))}>{t('common.retry')}</button>
    </aside>
  );
}
