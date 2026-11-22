/**
 * 统一图标集 —— 全部由微软官方 @fluentui/react-icons 提供（Fluent 2 图标系统）。
 * 保留本项目导出名与 <Icon size={n}/> 接口，调用方零改动。
 * 唯一例外：LogoMark 是 UFOT 品牌资产（几何三角徽记），不是通用图标，保留矢量自定义。
 */
import type { SVGProps } from 'react';
import {
  Chat20Regular, Send20Regular, Attach20Regular, Search20Regular,
  ArrowLeft20Regular, MoreHorizontal20Regular, Settings20Regular,
  Checkmark20Regular, CheckmarkCircle20Regular, Warning20Regular,
  WifiOff20Regular, Pin20Regular, MicOff20Regular, Add20Regular,
  Dismiss20Regular, Person20Regular, Image20Regular, Mic20Regular,
  Document20Regular, ArrowDownload20Regular, Play20Regular, Pause20Regular,
  WeatherSunny20Regular, WeatherMoon20Regular, Desktop20Regular,
  SignOut20Regular, Delete20Regular, Copy20Regular, ArrowUndo20Regular,
  ArrowRight20Regular, Flag20Regular, ChevronDown20Regular,
  SpinnerIosRegular, Call20Regular, Video20Regular, CallEnd20Filled,
  Speaker1Regular, CheckboxChecked20Regular,
} from '@fluentui/react-icons';

export interface IconProps extends SVGProps<SVGSVGElement> {
  size?: number;
}

// 官方图标固定 20px viewBox；用 size 做 CSS 缩放（不改 viewBox，保证描线一致）。
function wrap(Icon: React.ComponentType<{ className?: string }>) {
  return ({ size = 20, className = '', ...rest }: IconProps) => (
    <span
      className={`inline-flex shrink-0 items-center justify-center ${className}`}
      style={{ width: size, height: size }}
      aria-hidden="true"
      {...(rest as React.HTMLAttributes<HTMLSpanElement>)}
    >
      <Icon />
    </span>
  );
}

export const ChatIcon = wrap(Chat20Regular);
export const SendIcon = wrap(Send20Regular);
export const AttachIcon = wrap(Attach20Regular);
export const SearchIcon = wrap(Search20Regular);
export const BackIcon = wrap(ArrowLeft20Regular);
export const MoreIcon = wrap(MoreHorizontal20Regular);
export const SettingsIcon = wrap(Settings20Regular);
export const CheckIcon = wrap(Checkmark20Regular);
export const DoubleCheckIcon = wrap(CheckmarkCircle20Regular);
export const AlertIcon = wrap(Warning20Regular);
export const WifiOffIcon = wrap(WifiOff20Regular);
export const PinIcon = wrap(Pin20Regular);
export const MuteIcon = wrap(MicOff20Regular);
export const PlusIcon = wrap(Add20Regular);
export const CloseIcon = wrap(Dismiss20Regular);
export const UserIcon = wrap(Person20Regular);
export const ImageIcon = wrap(Image20Regular);
export const MicIcon = wrap(Mic20Regular);
export const FileIcon = wrap(Document20Regular);
export const DownloadIcon = wrap(ArrowDownload20Regular);
export const PlayIcon = wrap(Play20Regular);
export const PauseIcon = wrap(Pause20Regular);
export const SunIcon = wrap(WeatherSunny20Regular);
export const MoonIcon = wrap(WeatherMoon20Regular);
export const MonitorIcon = wrap(Desktop20Regular);
export const LogoutIcon = wrap(SignOut20Regular);
export const TrashIcon = wrap(Delete20Regular);
export const CopyIcon = wrap(Copy20Regular);
export const ReplyIcon = wrap(ArrowUndo20Regular);
export const ForwardIcon = wrap(ArrowRight20Regular);
export const FlagIcon = wrap(Flag20Regular);
export const ChevronDownIcon = wrap(ChevronDown20Regular);
export const SpinnerIcon = wrap(SpinnerIosRegular);
export const PhoneIcon = wrap(Call20Regular);
export const VideoIcon = wrap(Video20Regular);
export const HangupIcon = wrap(CallEnd20Filled);
export const SpeakerIcon = wrap(Speaker1Regular);
export const MicOffIcon = wrap(MicOff20Regular);
export const SelectIcon = wrap(CheckboxChecked20Regular);

/** UFOT 品牌字标：品牌资产，非通用图标，保留自定义矢量。 */
export const LogoMark = ({ size = 24, ...rest }: IconProps) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true" {...rest}>
    <path d="M12 2.6 21.8 20H2.2L12 2.6Z" fill="currentColor" fillOpacity="0.12" />
    <path d="M12 2.6 21.8 20H2.2L12 2.6Z" stroke="currentColor" strokeWidth="1.6" strokeLinejoin="round" />
    <path d="M12 9.4 17.4 19H6.6L12 9.4Z" stroke="currentColor" strokeWidth="1.2" strokeLinejoin="round" />
  </svg>
);
