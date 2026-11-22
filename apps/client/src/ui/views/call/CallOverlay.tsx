/**
 * 全屏通话覆盖层（§ C.1）。
 *   outgoing / connecting：呼叫中动画 + 对方昵称 + 取消按钮
 *   incoming：来电提示 + 接听 / 拒绝
 *   active：视频 = 远端全屏 + 本地小窗（固定右下角）；语音 = 头像 + 计时器 + 静音/免提/挂断
 * 统一 48px 圆按钮、暗色背景、aria-live 播报状态变化。
 */
import { useEffect, useRef, useState } from 'react';
import { useConvStore } from '../../../core/stores/conv-store';
import { useSessionStore } from '../../../core/stores/session-store';
import { t } from '../../i18n';
import { Button as FluentButton } from '@fluentui/react-components';
import { Avatar } from '../../components/Avatar';
import { IconButton } from '../../components/IconButton';
import { HangupIcon, MicIcon, MicOffIcon, PhoneIcon, SpeakerIcon, VideoIcon } from '../../components/icons';
import { useCall } from './useCall';

export function CallOverlay() {
  const call = useCall();
  const selfId = useSessionStore((state) => state.userId);
  const convId = call.info?.convId ?? null;
  const conv = useConvStore((state) => (convId ? state.convs[convId] : undefined));

  if (call.state === 'idle') return null;

  const peerId = call.info?.peerUserId ?? 0;
  const peer = conv?.members.find((member) => member.userId === peerId);
  const peerName = peer?.name || (peerId === selfId ? t('call.you') : t('common.unknownUser', { id: peerId }));
  const isVideo = call.info?.media === 'video';

  return (
    <div
      className="fixed inset-0 z-modal flex flex-col bg-gradient-to-b from-[#15171d] to-[#0b0d12] text-white"
      role="dialog"
      aria-modal="true"
      aria-label={t('call.active')}
    >
      <div className="sr-only" aria-live="polite" aria-atomic="true">
        {statusText(call.state, isVideo)}
      </div>

      {call.state === 'active' && isVideo ? (
        <VideoStage localStream={call.localStream} remoteStream={call.remoteStream} peerName={peerName} />
      ) : (
        <AudioStage
          state={call.state}
          isVideo={isVideo}
          peerName={peerName}
          peerId={peerId}
          startedAt={call.info?.startedAt}
        />
      )}

      <Controls
        state={call.state}
        isVideo={isVideo}
        muted={call.muted}
        onAccept={call.answer}
        onReject={call.reject}
        onHangup={call.hangup}
        onToggleMute={call.toggleMute}
      />
    </div>
  );
}

function statusText(state: string, isVideo: boolean): string {
  switch (state) {
    case 'outgoing':
      return isVideo ? t('call.callingVideo') : t('call.calling');
    case 'incoming':
      return isVideo ? t('call.incomingVideo') : t('call.incoming');
    case 'connecting':
      return t('call.connecting');
    case 'active':
      return t('call.active');
    case 'ended':
      return t('call.ended');
    default:
      return '';
  }
}

// ---------------------------------------------------------------------------
// 阶段视图
// ---------------------------------------------------------------------------

interface AudioStageProps {
  state: string;
  isVideo: boolean;
  peerName: string;
  peerId: number;
  startedAt?: number;
}

function AudioStage({ state, isVideo, peerName, peerId, startedAt }: AudioStageProps) {
  return (
    <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-4 px-6">
      <div className="relative">
        <Avatar name={peerName} size="xl" userId={peerId} className="ring-4 ring-white/10" />
        {state === 'outgoing' || state === 'connecting' || state === 'incoming' ? (
          <span className="absolute inset-0 -m-2 animate-ping rounded-full ring-2 ring-white/20" aria-hidden="true" />
        ) : null}
        {isVideo ? (
          <span className="absolute -bottom-2 -end-2 inline-flex h-7 w-7 items-center justify-center rounded-full bg-primary-500 text-white ring-2 ring-[#0b0d12]">
            <VideoIcon size={16} />
          </span>
        ) : null}
      </div>
      <p className="text-title text-white">{peerName}</p>
      <p className="text-body-sm text-white/60">{statusText(state, isVideo)}</p>
      {state === 'active' ? <CallTimer startedAt={startedAt} /> : null}
    </div>
  );
}

interface VideoStageProps {
  localStream: MediaStream | null;
  remoteStream: MediaStream | null;
  peerName: string;
}

function VideoStage({ localStream, remoteStream, peerName }: VideoStageProps) {
  return (
    <div className="relative min-h-0 flex-1 overflow-hidden bg-black">
      {remoteStream ? (
        <VideoSurface stream={remoteStream} className="h-full w-full object-cover" label={t('call.remoteVideo')} />
      ) : (
        <div className="flex h-full w-full flex-col items-center justify-center gap-3 text-white/70">
          <div className="h-16 w-16 animate-pulse rounded-full bg-white/10" />
          <p className="text-body-sm">{t('call.connecting')}</p>
        </div>
      )}

      <div className="absolute start-4 top-4 rounded-full border border-white/10 bg-black/40 px-3 py-1 text-caption text-white/80 backdrop-blur-sm">
        {peerName}
      </div>

      {/* 本地小窗：固定右下角 */}
      <div className="absolute bottom-28 end-4 h-40 w-28 overflow-hidden rounded-xl border border-white/20 bg-black shadow-lg tablet:h-48 tablet:w-36">
        {localStream ? (
          <VideoSurface
            stream={localStream}
            muted
            className="h-full w-full scale-x-[-1] object-cover"
            label={t('call.localPreview')}
          />
        ) : (
          <div className="flex h-full w-full items-center justify-center text-white/40">
            <MicIcon size={22} />
          </div>
        )}
      </div>

      <div className="absolute inset-x-0 top-0 h-16 bg-gradient-to-b from-black/40 to-transparent" aria-hidden="true" />
    </div>
  );
}

function VideoSurface({
  stream,
  muted = false,
  className = '',
  label,
}: {
  stream: MediaStream;
  muted?: boolean;
  className?: string;
  label: string;
}) {
  const ref = useRef<HTMLVideoElement | null>(null);
  useEffect(() => {
    const element = ref.current;
    if (!element) return;
    element.srcObject = stream;
    void element.play().catch(() => undefined);
    return () => {
      element.srcObject = null;
    };
  }, [stream]);
  return <video ref={ref} playsInline autoPlay muted={muted} aria-label={label} className={className} />;
}

function CallTimer({ startedAt }: { startedAt?: number }) {
  const [now, setNow] = useState(() => Date.now());
  const base = useRef(startedAt ?? Date.now());
  useEffect(() => {
    base.current = startedAt ?? Date.now();
  }, [startedAt]);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  const elapsed = Math.max(0, Math.floor((now - base.current) / 1000));
  const mm = Math.floor(elapsed / 60);
  const ss = elapsed % 60;
  return <p className="nums text-body-sm text-white/70">{`${String(mm).padStart(2, '0')}:${String(ss).padStart(2, '0')}`}</p>;
}

// ---------------------------------------------------------------------------
// 控制按钮
// ---------------------------------------------------------------------------

interface ControlsProps {
  state: string;
  isVideo: boolean;
  muted: boolean;
  onAccept: () => void;
  onReject: () => void;
  onHangup: () => void;
  onToggleMute: () => void;
}

function Controls({ state, isVideo, muted, onAccept, onReject, onHangup, onToggleMute }: ControlsProps) {
  const round = 'h-12 w-12 rounded-full';

  if (state === 'incoming') {
    return (
      <div className="flex items-center justify-center gap-16 px-6 pb-10">
        <div className="flex flex-col items-center gap-2">
          <IconButton
            label={t('call.reject')}
            icon={<HangupIcon size={22} />}
            onClick={onReject}
            className={`${round} bg-danger-500 text-white`}
          />
          <span className="text-caption text-white/70">{t('call.reject')}</span>
        </div>
        <div className="flex flex-col items-center gap-2">
          <IconButton
            label={t('call.accept')}
            icon={isVideo ? <VideoIcon size={22} /> : <PhoneIcon size={22} />}
            onClick={onAccept}
            className={`${round} bg-success-600 text-white`}
          />
          <span className="text-caption text-white/70">{t('call.accept')}</span>
        </div>
      </div>
    );
  }

  const canMute = state === 'active';
  return (
    <div className="flex items-center justify-center gap-10 px-6 pb-10">
      {canMute ? (
        <div className="flex flex-col items-center gap-2">
          <FluentButton
            appearance="subtle"
            aria-label={muted ? t('call.unmute') : t('call.mute')}
            title={muted ? t('call.unmute') : t('call.mute')}
            aria-pressed={muted}
            icon={muted ? <MicOffIcon size={22} /> : <MicIcon size={22} />}
            onClick={onToggleMute}
            className={`${round} ${muted ? 'bg-white text-[#0b0d12]' : 'bg-white/15 text-white'}`}
          />
          <span className="text-caption text-white/70">{muted ? t('call.muted') : t('call.mute')}</span>
        </div>
      ) : null}

      {canMute && !isVideo ? (
        <div className="flex flex-col items-center gap-2">
          <IconButton
            label={t('call.speaker')}
            icon={<SpeakerIcon size={22} />}
            className={`${round} bg-white/15 text-white`}
          />
          <span className="text-caption text-white/70">{t('call.speaker')}</span>
        </div>
      ) : null}

      <div className="flex flex-col items-center gap-2">
        <IconButton
          label={t('call.hangup')}
          icon={<HangupIcon size={22} />}
          onClick={onHangup}
          className={`${round} bg-danger-500 text-white`}
        />
        <span className="text-caption text-white/70">{state === 'active' ? t('call.hangup') : t('call.cancel')}</span>
      </div>
    </div>
  );
}
