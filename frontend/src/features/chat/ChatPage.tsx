/** 会话详情：顶栏、消息流、输入区。 */

import { useEffect, useRef, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { Avatar, Button, EmptyState, IconButton, TextInput } from '@/ui/primitives/controls';
import { Dialog } from '@/ui/primitives/Dialog';
import { MessageList } from './MessageList';
import { Composer } from './Composer';
import { useConvStore } from '@/core/store/conv';
import { useMsgStore } from '@/core/store/msg';
import { useAuthStore } from '@/core/store/auth';
import { useUiStore } from '@/core/store/ui';
import { requestSync, sendReceipt, sendMutation, sendForward, sendMessage, sendEdit } from '@/core/store/bridge';

const NO_TYPING: readonly number[] = [];

export function ChatPage() {
  const { convId: rawConvId } = useParams();
  const convId = Number(rawConvId ?? 0);
  const navigate = useNavigate();
  const conv = useConvStore((s) => s.conversations[convId]);
  const messages = useMsgStore((s) => s.messages[convId]);
  const typingRaw = useMsgStore((s) => s.typing[convId]);
  const typing = typingRaw ?? NO_TYPING;
  const userId = useAuthStore((s) => s.session?.user_id ?? 0);
  const pushToast = useUiStore((s) => s.pushToast);
  const historyLoaded = useRef<Set<number>>(new Set());
  const [infoOpen, setInfoOpen] = useState(false);

  const list = messages ?? [];
  const lastMsgId = list.length ? list[list.length - 1]?.msgId ?? 0 : 0;

  // 首次进入：拉一次历史（sinceSeq=0，服务端按 seq 升序逐条回 102）。
  useEffect(() => {
    if (!convId) return;
    if (historyLoaded.current.has(convId)) return;
    historyLoaded.current.add(convId);
    requestSync(convId, 0, 200);
  }, [convId]);

  // 进入会话后把最新一条标记为已读（回执走 104）。
  useEffect(() => {
    if (!lastMsgId || !convId) return;
    const last = list[list.length - 1];
    if (!last || last.senderId === userId) return;
    sendReceipt(convId, lastMsgId, 0);
    useMsgStore.getState().markRead(convId, lastMsgId);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lastMsgId, convId, userId]);

  const title = conv?.title || (convId ? `会话 ${convId}` : '会话');

  const handleRecall = (msgId: number) => {
    if (!window.confirm('撤回后所有人都会看到「消息已撤回」。确定撤回吗？')) return;
    sendMutation(convId, msgId, true);
    useMsgStore.getState().applyTombstone(convId, msgId, true);
    pushToast('正在撤回', 'info');
  };

  const handleDelete = (msgId: number) => {
    if (!window.confirm('删除后这条消息将不再显示。确定删除吗？')) return;
    sendMutation(convId, msgId, false);
    useMsgStore.getState().applyTombstone(convId, msgId, true);
    pushToast('已删除', 'info');
  };

  const [editOpen, setEditOpen] = useState(false);
  const [editMsgId, setEditMsgId] = useState(0);
  const [editText, setEditText] = useState('');
  const [forwardOpen, setForwardOpen] = useState(false);
  const [forwardMsgId, setForwardMsgId] = useState(0);
  const [forwardTarget, setForwardTarget] = useState(0);

  const handleEdit = (msgId: number, text: string) => {
    setEditMsgId(msgId);
    setEditText(text);
    setEditOpen(true);
  };

  const submitEdit = () => {
    if (!editText.trim() || !editMsgId) return;
    sendEdit(convId, editMsgId, editText.trim());
    useMsgStore.getState().updateText(convId, editMsgId, editText.trim());
    setEditOpen(false);
    setEditMsgId(0);
  };

  const handleForward = (msgId: number) => {
    setForwardMsgId(msgId);
    const others = Object.values(useConvStore.getState().conversations).filter((c) => c.convId !== convId);
    setForwardTarget(others[0]?.convId ?? 0);
    if (!others.length) {
      pushToast('暂时没有其他会话可以转发', 'warn');
      return;
    }
    setForwardOpen(true);
  };

  const submitForward = () => {
    if (!forwardTarget || !forwardMsgId) return;
    sendForward(convId, forwardTarget, forwardMsgId);
    const target = useConvStore.getState().conversations[forwardTarget];
    pushToast(`已转发到「${target?.title || forwardTarget}」`, 'info');
    setForwardOpen(false);
  };

  const handleRetry = (msgId: number) => {
    const message = list.find((m) => m.msgId === msgId);
    if (!message) return;
    useMsgStore.getState().setStatus(convId, msgId, 'pending');
    const ok = sendMessage({
      convId,
      text: message.text,
      media: message.media,
      replyTo: message.replyTo,
      mentions: message.mentions,
      clientMsgId: message.clientMsgId ?? `retry-${Date.now()}`,
    });
    if (!ok) useMsgStore.getState().setStatus(convId, msgId, 'failed');
  };

  if (!convId) {
    return <EmptyState icon="chat" title="会话不存在" description="请从会话列表中选择一个会话。" />;
  }

  return (
    <>
      <header className="uf-topbar">
        <IconButton
          icon="back"
          label="返回会话列表"
          className="uf-only-mobile"
          onClick={() => navigate('/chats')}
        />
        <Avatar name={title} size="sm" />
        <div className="uf-col uf-grow">
          <span className="uf-topbar__title truncate">{title}</span>
          <span className="uf-topbar__sub">
            {conv?.memberCount ? `${conv.memberCount} 名成员` : ''}
            {typing.length > 0 ? ' · 正在输入…' : ''}
          </span>
        </div>
        <IconButton icon="info" label="会话信息" onClick={() => setInfoOpen(true)} />
      </header>

      <ConversationInfoDialog convId={convId} open={infoOpen} onClose={() => setInfoOpen(false)} />

      <MessageList
        messages={list}
        currentUserId={userId}
        onRecall={handleRecall}
        onDelete={handleDelete}
        onForward={handleForward}
        onEdit={handleEdit}
        onRetry={handleRetry}
      />

      <Dialog open={editOpen} onClose={() => setEditOpen(false)} title="编辑消息"
        actions={<><Button onClick={() => setEditOpen(false)}>取消</Button><Button variant="primary" onClick={submitEdit}>保存</Button></>}>
        <TextInput value={editText} onChange={(e) => setEditText(e.target.value)} maxLength={2000} aria-label="消息内容" />
      </Dialog>

      <Dialog open={forwardOpen} onClose={() => setForwardOpen(false)} title="转发到"
        actions={<><Button onClick={() => setForwardOpen(false)}>取消</Button><Button variant="primary" disabled={!forwardTarget} onClick={submitForward}>转发</Button></>}>
        <div className="uf-col" style={{ gap: 'var(--sp-2)' }}>
          {Object.values(useConvStore.getState().conversations)
            .filter((c) => c.convId !== convId)
            .map((c) => (
              <button key={c.convId} type="button" className="uf-conv" onClick={() => setForwardTarget(c.convId)}
                style={{ background: forwardTarget === c.convId ? 'var(--surface-2)' : undefined }}>
                <span className="uf-grow truncate">{c.title || `会话 ${c.convId}`}</span>
                {forwardTarget === c.convId ? <Icon name="check" size="sm" /> : null}
              </button>
            ))}
        </div>
      </Dialog>

      <Composer convId={convId} />
    </>
  );
}

/** 会话信息占位弹窗（成员、置顶、静音等操作入口）。 */
export function ConversationInfoDialog({
  convId,
  open,
  onClose,
}: {
  convId: number;
  open: boolean;
  onClose: () => void;
}) {
  const conv = useConvStore((s) => s.conversations[convId]);
  const togglePinned = useConvStore((s) => s.togglePinned);
  const toggleMuted = useConvStore((s) => s.toggleMuted);
  const pushToast = useUiStore.getState().pushToast;

  return (
    <Dialog open={open} onClose={onClose} title="会话信息">
      <div className="uf-col" style={{ gap: 'var(--sp-3)' }}>
        <div className="uf-row">
          <Avatar name={conv?.title} size="lg" />
          <div className="uf-col">
            <strong>{conv?.title || `会话 ${convId}`}</strong>
            <span className="uf-faint">ID：{convId}</span>
          </div>
        </div>
        <hr className="uf-divider" />
        <div className="uf-row">
          <Button
            icon="list"
            onClick={() => {
              togglePinned(convId);
              pushToast(conv?.pinned ? '已取消置顶' : '已置顶', 'info');
            }}
          >
            {conv?.pinned ? '取消置顶' : '置顶'}
          </Button>
          <Button
            icon="bell"
            onClick={() => {
              toggleMuted(convId);
              pushToast(conv?.muted ? '已取消静音' : '已静音', 'info');
            }}
          >
            {conv?.muted ? '取消静音' : '静音'}
          </Button>
        </div>
        {conv?.members?.length ? (
          <>
            <hr className="uf-divider" />
            <strong className="uf-label">成员（{conv.members.length}）</strong>
            {conv.members.map((member) => (
              <div key={member.userId} className="uf-row">
                <Avatar name={`U${member.userId}`} size="sm" />
                <span className="uf-grow">成员 {member.userId}</span>
                <span className="uf-faint">{member.role === 1 ? '群主' : '成员'}</span>
              </div>
            ))}
          </>
        ) : null}
      </div>
    </Dialog>
  );
}
