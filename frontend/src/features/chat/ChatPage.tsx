/** 会话详情：顶栏、消息流、输入区。 */

import { useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { Avatar, Button, EmptyState, IconButton } from '@/ui/primitives/controls';
import { Dialog } from '@/ui/primitives/Dialog';
import { MessageList } from './MessageList';
import { Composer } from './Composer';
import { useConvStore } from '@/core/store/conv';
import { useMsgStore } from '@/core/store/msg';
import { useAuthStore } from '@/core/store/auth';
import { useUiStore } from '@/core/store/ui';
import { requestSync, sendReceipt, sendMutation, sendForward } from '@/core/store/bridge';

export function ChatPage() {
  const { convId: rawConvId } = useParams();
  const convId = Number(rawConvId ?? 0);
  const navigate = useNavigate();
  const conv = useConvStore((s) => s.conversations[convId]);
  const messages = useMsgStore((s) => s.messages[convId]);
  const typing = useMsgStore((s) => s.typing[convId] ?? []);
  const userId = useAuthStore((s) => s.session?.user_id ?? 0);
  const pushToast = useUiStore((s) => s.pushToast);
  const historyLoaded = useRef<Set<number>>(new Set());
  const [infoOpen, setInfoOpen] = useState(false);

  const list = useMemo(() => messages ?? [], [messages]);

  // 首次进入：拉一次历史（sinceSeq=0，服务端按 seq 升序逐条回 102）。
  useEffect(() => {
    if (!convId) return;
    if (historyLoaded.current.has(convId)) return;
    historyLoaded.current.add(convId);
    requestSync(convId, 0, 200);
  }, [convId]);

  // 进入会话后把最新一条标记为已读（回执走 104）。
  useEffect(() => {
    const last = list[list.length - 1];
    if (!last || last.senderId === userId) return;
    sendReceipt(convId, last.msgId, 0);
    useMsgStore.getState().markRead(convId, last.msgId);
  }, [list, convId, userId]);

  const title = conv?.title || (convId ? `会话 ${convId}` : '会话');

  const handleRecall = (msgId: number) => {
    sendMutation(convId, msgId, true);
    useMsgStore.getState().applyTombstone(convId, msgId, true);
    pushToast('已请求撤回', 'info');
  };

  const handleDelete = (msgId: number) => {
    sendMutation(convId, msgId, false);
    useMsgStore.getState().applyTombstone(convId, msgId, true);
    pushToast('已删除', 'info');
  };

  const handleForward = (msgId: number) => {
    const target = Object.values(useConvStore.getState().conversations).find(
      (c) => c.convId !== convId,
    );
    if (!target) {
      pushToast('没有可转发的目标会话', 'warn');
      return;
    }
    sendForward(convId, target.convId, msgId);
    pushToast(`已转发到「${target.title || target.convId}」`, 'info');
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
            {typing.length > 0 ? ' · 对方正在输入' : ''}
          </span>
        </div>
        <IconButton icon="info" label="会话信息" onClick={() => setInfoOpen(true)} />
      </header>

      <ConversationInfoDialog convId={convId} open={infoOpen} onClose={() => setInfoOpen(false)} />

      <MessageList
        convId={convId}
        messages={list}
        currentUserId={userId}
        onRecall={handleRecall}
        onDelete={handleDelete}
        onForward={handleForward}
      />

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
  const pushToast = useUiStore((s) => s.pushToast);

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
                <span className="uf-grow">用户 {member.userId}</span>
                <span className="uf-faint">{member.role === 1 ? '群主' : '成员'}</span>
              </div>
            ))}
          </>
        ) : null}
      </div>
    </Dialog>
  );
}
