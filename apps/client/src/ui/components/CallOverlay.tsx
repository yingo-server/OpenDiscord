import { useEffect, useState, type ReactNode } from 'react';
import { callSession } from '../../net/ws/call';
import { t } from '../i18n';

export function CallSettingsRow(): ReactNode {
  const enabled = callSession.enabled;
  return (
    <div className="settings-row">
      <span className="label">{t('call.title')}<small>{enabled ? '' : t('call.unavailable')}</small></span>
      <button className="btn small" type="button" disabled={!enabled}>{t('call.title')}</button>
    </div>
  );
}

export function CallOverlay(): ReactNode {
  const [call, setCall] = useState(callSession.call);
  const [remote, setRemote] = useState<MediaStream | null>(null);
  useEffect(() => callSession.subscribe(setCall), []);
  useEffect(() => callSession.onRemoteStream(setRemote), []);

  const audioRef = (node: HTMLAudioElement | null) => {
    if (node && remote) {
      node.srcObject = remote;
      node.play().catch(() => undefined);
    }
  };

  if (call.state === 'idle') return null;
  const label = call.state === 'incoming'
    ? `${call.peerName} 请求通话`
    : call.state === 'outgoing' ? '正在呼叫…'
      : call.state === 'active' ? '通话中' : '';

  return (
    <div className="call-overlay" role="dialog" aria-modal="true" aria-label={t('call.title')}>
      <div className="call-card">
        <div className="call-peer">{call.peerName || t('call.title')}</div>
        <div className="call-state">{label}</div>
        {call.state === 'active' ? <audio ref={audioRef} autoPlay /> : null}
        <div className="call-actions">
          {call.state === 'incoming' ? (
            <>
              <button className="btn" type="button" onClick={() => void callSession.accept()}>{t('call.accept')}</button>
              <button className="btn danger" type="button" onClick={() => void callSession.reject()}>{t('call.reject')}</button>
            </>
          ) : (
            <>
              <button className="btn" type="button" onClick={() => callSession.toggleMute()}>{call.muted ? t('call.unmute') : t('call.mute')}</button>
              <button className="btn" type="button" onClick={() => callSession.toggleSpeaker()}>{call.speaker ? t('call.speaker.off') : t('call.speaker')}</button>
              <button className="btn danger" type="button" onClick={() => callSession.hangup()}>{t('call.hangup')}</button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
