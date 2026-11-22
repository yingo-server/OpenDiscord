/**
 * 语音渲染器（§4.3）：固定 24 段波形、宽度随时长（60s 封顶）、
 * 播放进度高亮、当前段 1.5× 呼吸、同会话仅一条在播、未读红点。
 * 无真实音频地址时按时长模拟播放（演示/弱网降级）。
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { selectIsMine } from '../../core/selectors';
import { useSessionStore } from '../../core/stores/session-store';
import type { Media } from '../../net/proto/payload';
import { t } from '../i18n';
import { PauseIcon, PlayIcon } from '../components/icons';
import { formatDuration } from '../utils/format';
import { resolveMediaUrl } from '../../net/media';
import { UnknownMessage } from './unknown';
import type { RendererProps } from './registry';

/** 全局唯一播放器：开始新语音时停止旧的 */
let activeStop: (() => void) | null = null;

export function AudioMessage({ message }: RendererProps) {
  const userId = useSessionStore((state) => state.userId);
  const media = message.payload.media?.[0];
  const mine = selectIsMine(message, userId);
  if (!media) return <UnknownMessage message={message} />;
  const audioUrl = resolveMediaUrl(media);
  return <AudioBody media={media} mine={mine} src={audioUrl} key={`${media.mediaId}-${message.id}`} />;
}

function AudioBody({ media, mine, src }: { media: Media; mine: boolean; src?: string }) {
  const duration = Math.max(1000, media.durationMs || 1000);
  const waveform = useMemo(() => {
    if (media.waveform?.length === 24) {
      // 统一 0-100 量纲（服务端校验区间）：渲染用 0-1，除以 100。
      return media.waveform.map((value) => Math.max(0, Math.min(1, value / 100)));
    }
    return pseudoWaveform(media.mediaId, duration);
  }, [media.mediaId, media.waveform, duration]);
  const [playing, setPlaying] = useState(false);
  const [progress, setProgress] = useState(0);
  const [played, setPlayed] = useState(false);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const progressRef = useRef(0);
  progressRef.current = progress;

  const stop = useCallback(() => {
    if (timerRef.current) {
      clearInterval(timerRef.current);
      timerRef.current = null;
    }
    if (audioRef.current) {
      audioRef.current.pause();
      audioRef.current = null;
    }
    setPlaying(false);
  }, []);

  useEffect(() => {
    const selfStop = () => stop();
    return () => {
      if (activeStop === selfStop) activeStop = null;
      stop();
    };
  }, [stop]);

  const simulate = useCallback(
    (from: number) => {
      const startedAt = Date.now() - from * duration;
      timerRef.current = setInterval(() => {
        const ratio = (Date.now() - startedAt) / duration;
        if (ratio >= 1) {
          if (timerRef.current) clearInterval(timerRef.current);
          timerRef.current = null;
          setProgress(0);
          setPlaying(false);
          return;
        }
        setProgress(ratio);
      }, 100);
    },
    [duration],
  );

  const toggle = useCallback(() => {
    if (playing) {
      stop();
      return;
    }
    activeStop?.();
    const selfStop = () => stop();
    activeStop = selfStop;
    setPlayed(true);
    setPlaying(true);
    const from = progressRef.current >= 0.999 ? 0 : progressRef.current;
    setProgress(from);

    if (src) {
      try {
        const audio = new Audio(src);
        audio.currentTime = (from * duration) / 1000;
        audioRef.current = audio;
        audio.ontimeupdate = () => {
          const total = Number.isFinite(audio.duration) && audio.duration > 0 ? audio.duration : duration / 1000;
          setProgress(Math.min(1, audio.currentTime / total));
        };
        audio.onended = () => {
          stop();
          setProgress(0);
        };
        void audio.play().catch(() => simulate(from));
        return;
      } catch {
        /* fallthrough: 模拟播放 */
      }
    }
    simulate(from);
  }, [duration, src, playing, simulate, stop]);

  const width = Math.min(220, Math.max(96, 96 + (duration / 60_000) * 124));
  const activeIndex = Math.floor(progress * 24);
  const short = duration <= 2_000;

  return (
    <span className="flex items-center gap-2" style={{ width }}>
      <button
        type="button"
        onClick={toggle}
        aria-label={playing ? t('media.pause') : t('media.play')}
        className="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-ink/[0.08] transition-colors duration-micro hover:bg-ink/[0.14]"
      >
        {playing ? <PauseIcon size={14} /> : <PlayIcon size={14} />}
      </button>

      {short ? (
        <span className="nums flex-1 text-body-sm opacity-80">{formatDuration(duration)}</span>
      ) : (
        <span className="flex h-5 flex-1 items-center gap-[2px]" aria-hidden="true">
          {waveform.map((value, index) => (
            <span
              key={index}
              className={[
                'w-[2px] rounded-full bg-current transition-opacity duration-micro',
                index <= activeIndex && (playing || progress > 0) ? 'opacity-90' : 'opacity-30',
                playing && index === activeIndex ? 'animate-wave-breath origin-center' : '',
              ].join(' ')}
              style={{ height: `${Math.round(3 + value * 17)}px` }}
            />
          ))}
        </span>
      )}

      <span className="nums shrink-0 text-caption opacity-70">{formatDuration(duration)}</span>
      {!mine && !played ? (
        <span className="h-2 w-2 shrink-0 rounded-full bg-danger-500" aria-label="unread" />
      ) : null}
    </span>
  );
}

function pseudoWaveform(key: string, duration: number): number[] {
  let h = Math.abs(hash(key || String(duration)));
  const values: number[] = [];
  for (let i = 0; i < 24; i += 1) {
    h = (h * 1_103_515_245 + 12_345) & 0x7fffffff;
    values.push(0.18 + ((h >> 8) % 78) / 100);
  }
  return values;
}

function hash(value: string): number {
  let h = 0;
  for (let i = 0; i < value.length; i += 1) h = (h * 31 + value.charCodeAt(i)) | 0;
  return h;
}
