/**
 * 统一图标集：24×24 网格、1.5px 描线、currentColor。
 * 不引第三方图标库，避免包体与风格漂移。
 */
import type { SVGProps } from 'react';

export interface IconProps extends SVGProps<SVGSVGElement> {
  size?: number;
}

function Svg({ size = 24, children, ...rest }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      {...rest}
    >
      {children}
    </svg>
  );
}

export const ChatIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M20 11.5a7.8 7.8 0 0 1-8 7.8 9 9 0 0 1-2.6-.4L4 20.5l1.5-4.3A7.6 7.6 0 0 1 4 11.5 7.8 7.8 0 0 1 12 3.7a7.8 7.8 0 0 1 8 7.8Z" />
  </Svg>
);

/** UFOT 品牌字标：24 网格内的几何三角徽记，当前色继承强调色。 */
export const LogoMark = ({ size = 24, ...rest }: IconProps) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    aria-hidden="true"
    {...rest}
  >
    <path d="M12 2.6 21.8 20H2.2L12 2.6Z" fill="currentColor" fillOpacity="0.12" />
    <path d="M12 2.6 21.8 20H2.2L12 2.6Z" stroke="currentColor" strokeWidth="1.6" strokeLinejoin="round" />
    <path d="M12 9.4 17.4 19H6.6L12 9.4Z" stroke="currentColor" strokeWidth="1.2" strokeLinejoin="round" />
  </svg>
);

export const SendIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M4 12 20 4l-3.5 16L12 14l-8-2Z" />
    <path d="M12 14 20 4" />
  </Svg>
);

export const AttachIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m20 11.5-7.6 7.6a5 5 0 0 1-7-7l8-8a3.4 3.4 0 0 1 4.8 4.8l-8 8a1.8 1.8 0 0 1-2.5-2.5l7.2-7.2" />
  </Svg>
);

export const SearchIcon = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="11" cy="11" r="6.5" />
    <path d="m16 16 4 4" />
  </Svg>
);

export const BackIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m14.5 5-7 7 7 7" />
  </Svg>
);

export const MoreIcon = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="5" cy="12" r="1" fill="currentColor" />
    <circle cx="12" cy="12" r="1" fill="currentColor" />
    <circle cx="19" cy="12" r="1" fill="currentColor" />
  </Svg>
);

export const SettingsIcon = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="3" />
    <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06-2.83 2.83-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1.03 1.56V21h-4v-.08A1.7 1.7 0 0 0 9 19.36a1.7 1.7 0 0 0-1.87.34l-.06.06-2.83-2.83.06-.06A1.7 1.7 0 0 0 4.64 15 1.7 1.7 0 0 0 3.08 14H3v-4h.08A1.7 1.7 0 0 0 4.64 9a1.7 1.7 0 0 0-.34-1.87l-.06-.06 2.83-2.83.06.06A1.7 1.7 0 0 0 9 4.64 1.7 1.7 0 0 0 10 3.08V3h4v.08A1.7 1.7 0 0 0 15 4.64a1.7 1.7 0 0 0 1.87-.34l.06-.06 2.83 2.83-.06.06A1.7 1.7 0 0 0 19.36 9c.18.4.56.73 1.56.92V14h-.08A1.7 1.7 0 0 0 19.4 15Z" />
  </Svg>
);

export const CheckIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m5 12.5 4.2 4.2L19 6.8" />
  </Svg>
);

export const DoubleCheckIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m2 12.6 3.8 3.9L14 8.2" />
    <path d="m9.5 12.6 3.8 3.9L21.6 8.2" />
  </Svg>
);

export const AlertIcon = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="8.5" />
    <path d="M12 7.5v5M12 16.2v.2" />
  </Svg>
);

export const WifiOffIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m3 3 18 18" />
    <path d="M8.5 8.6A10 10 0 0 1 21 9.8M3 9.8a10 10 0 0 1 2.2-1.2M6.5 13.2a6.5 6.5 0 0 1 3-1.2M12 17.6h.01M10.5 13.9a3 3 0 0 1 3.4 1" />
  </Svg>
);

export const PinIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m9 4 6 0-1 5 3 3v2H7v-2l3-3-1-5Z" />
    <path d="M12 14v6" />
  </Svg>
);

export const MuteIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M6 9a6 6 0 0 1 12 0c0 3 1 4.5 1.5 5.5H4.5C5 13.5 6 12 6 9Z" />
    <path d="M10 18a2.2 2.2 0 0 0 4 0M3 3l18 18" />
  </Svg>
);

export const PlusIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M12 5v14M5 12h14" />
  </Svg>
);

export const CloseIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m6 6 12 12M18 6 6 18" />
  </Svg>
);

export const UserIcon = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="12" cy="8.5" r="3.5" />
    <path d="M5 20a7 7 0 0 1 14 0" />
  </Svg>
);

export const ImageIcon = (p: IconProps) => (
  <Svg {...p}>
    <rect x="3.5" y="4.5" width="17" height="15" rx="3" />
    <circle cx="9" cy="10" r="1.5" />
    <path d="m5 17 4.5-4 3.5 3 2.5-2 3.5 3" />
  </Svg>
);

export const MicIcon = (p: IconProps) => (
  <Svg {...p}>
    <rect x="9" y="3.5" width="6" height="10" rx="3" />
    <path d="M6 11.5a6 6 0 0 0 12 0M12 17.5V21" />
  </Svg>
);

export const FileIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M7 3.5h7l4 4v13H7z" />
    <path d="M14 3.5v4h4M9.5 12h5M9.5 15.5h5" />
  </Svg>
);

export const DownloadIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M12 4v10M8 10.5l4 4 4-4M5 19.5h14" />
  </Svg>
);

export const PlayIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M8 5.5 19 12 8 18.5z" fill="currentColor" stroke="none" />
  </Svg>
);

export const PauseIcon = (p: IconProps) => (
  <Svg {...p}>
    <rect x="7" y="5.5" width="3.5" height="13" rx="1" fill="currentColor" stroke="none" />
    <rect x="13.5" y="5.5" width="3.5" height="13" rx="1" fill="currentColor" stroke="none" />
  </Svg>
);

export const SunIcon = (p: IconProps) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5 5l1.4 1.4M17.6 17.6 19 19M19 5l-1.4 1.4M6.4 17.6 5 19" />
  </Svg>
);

export const MoonIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z" />
  </Svg>
);

export const MonitorIcon = (p: IconProps) => (
  <Svg {...p}>
    <rect x="3" y="4.5" width="18" height="12" rx="2" />
    <path d="M8 20.5h8M12 16.5v4" />
  </Svg>
);

export const LogoutIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M9 4.5H5.5v15H9M14 8l4 4-4 4M8 12h10" />
  </Svg>
);

export const TrashIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M4.5 7h15M9 7V4.5h6V7M6 7l1 13h10l1-13M10 11v5M14 11v5" />
  </Svg>
);

export const CopyIcon = (p: IconProps) => (
  <Svg {...p}>
    <rect x="8.5" y="8.5" width="11" height="12" rx="2" />
    <path d="M15.5 8.5v-2a2 2 0 0 0-2-2h-8a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h2" />
  </Svg>
);

export const ReplyIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M9 7 4 12l5 5" />
    <path d="M4 12h8a7 7 0 0 1 7 7v0" />
  </Svg>
);

export const ForwardIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m15 7 5 5-5 5" />
    <path d="M20 12h-8a7 7 0 0 0-7 7v0" />
  </Svg>
);

export const SelectIcon = (p: IconProps) => (
  <Svg {...p}>
    <rect x="4" y="4" width="16" height="16" rx="3" />
    <path d="m8 12 2.5 2.5L16 9" />
  </Svg>
);

export const FlagIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M5 21V4M5 4h11l-1.5 4L16 12H5" />
  </Svg>
);

export const ChevronDownIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="m6 9 6 6 6-6" />
  </Svg>
);

export const SpinnerIcon = ({ size = 16, className = '', ...rest }: IconProps) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    className={`animate-spin ${className}`}
    aria-hidden="true"
    {...rest}
  >
    <circle cx="12" cy="12" r="8.5" stroke="currentColor" strokeOpacity="0.25" strokeWidth="2.5" />
    <path d="M20.5 12a8.5 8.5 0 0 0-8.5-8.5" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" />
  </svg>
);
export const PhoneIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M6.5 3.5h3l1.5 4-2 1.5a12 12 0 0 0 5 5l1.5-2 4 1.5v3a2 2 0 0 1-2 2A16.5 16.5 0 0 1 4.5 5.5a2 2 0 0 1 2-2Z" />
  </Svg>
);

export const VideoIcon = (p: IconProps) => (
  <Svg {...p}>
    <rect x="3" y="6.5" width="12" height="11" rx="2.5" />
    <path d="m15 10.5 5-3v9l-5-3" />
  </Svg>
);

export const HangupIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M3.5 14.5c5-3.5 12-3.5 17 0l-2 2.2-2.6-1.3-.4-2.2a11 11 0 0 0-7 0l-.4 2.2-2.6 1.3-2-2.2Z" />
  </Svg>
);

export const SpeakerIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M4 9.5h3l4-3.5v12l-4-3.5H4Z" />
    <path d="M15.5 9a4 4 0 0 1 0 6M18 6.5a7.5 7.5 0 0 1 0 11" />
  </Svg>
);

export const MicOffIcon = (p: IconProps) => (
  <Svg {...p}>
    <path d="M9 9.2V6.5a3 3 0 0 1 5.6-1.5M15 12.5a3 3 0 0 1-4.8 2.4" />
    <path d="M6 11.5a6 6 0 0 0 9 5.2M12 17.5V21M3 3l18 18" />
  </Svg>
);

