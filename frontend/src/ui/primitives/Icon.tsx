/**
 * 图标：Font Awesome 6 Free（npm 自托管，不走 CDN），禁 emoji。
 * 这里维护一张**受控图标清单**，避免各业务直接 import 图标库导致体积失控。
 */

import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import type { IconDefinition } from '@fortawesome/fontawesome-svg-core';
import type { CSSProperties } from 'react';
import {
  faArrowLeft,
  faArrowRight,
  faArrowUpRightFromSquare,
  faBan,
  faBell,
  faChartLine,
  faCheck,
  faCheckDouble,
  faCircleInfo,
  faClock,
  faComments,
  faEllipsis,
  faEllipsisVertical,
  faFile,
  faGear,
  faImage,
  faLink,
  faListUl,
  faLock,
  faMagnifyingGlass,
  faMicrophone,
  faPaperPlane,
  faPaperclip,
  faPen,
  faPhone,
  faPlus,
  faReply,
  faRotateRight,
  faShieldHalved,
  faSignOutAlt,
  faSpinner,
  faSun,
  faMoon,
  faTriangleExclamation,
  faTrash,
  faUser,
  faUserPlus,
  faUsers,
  faWifi,
  faXmark,
  faFlag,
  faBroadcastTower,
  faDatabase,
  faScroll,
} from '@fortawesome/free-solid-svg-icons';

const ICONS = {
  back: faArrowLeft,
  forward: faArrowRight,
  open: faArrowUpRightFromSquare,
  ban: faBan,
  bell: faBell,
  metrics: faChartLine,
  check: faCheck,
  checkDouble: faCheckDouble,
  info: faCircleInfo,
  clock: faClock,
  chat: faComments,
  more: faEllipsis,
  moreVertical: faEllipsisVertical,
  file: faFile,
  settings: faGear,
  image: faImage,
  link: faLink,
  list: faListUl,
  lock: faLock,
  search: faMagnifyingGlass,
  mic: faMicrophone,
  send: faPaperPlane,
  attach: faPaperclip,
  edit: faPen,
  call: faPhone,
  plus: faPlus,
  reply: faReply,
  refresh: faRotateRight,
  shield: faShieldHalved,
  logout: faSignOutAlt,
  spinner: faSpinner,
  sun: faSun,
  moon: faMoon,
  warning: faTriangleExclamation,
  trash: faTrash,
  user: faUser,
  userPlus: faUserPlus,
  group: faUsers,
  online: faWifi,
  close: faXmark,
  report: faFlag,
  broadcast: faBroadcastTower,
  archive: faDatabase,
  audit: faScroll,
} satisfies Record<string, IconDefinition>;

export type IconName = keyof typeof ICONS;

export interface IconProps {
  name: IconName;
  size?: 'sm' | 'md' | 'lg';
  className?: string;
  style?: CSSProperties;
  title?: string;
  spin?: boolean;
}

const SIZE_MAP = { sm: 'var(--icon-sm)', md: 'var(--icon-md)', lg: 'var(--icon-lg)' } as const;

export function Icon({ name, size = 'md', className, style, title, spin }: IconProps) {
  const definition = ICONS[name];
  if (!definition) return null;
  return (
    <FontAwesomeIcon
      icon={definition}
      className={className}
      style={{ fontSize: SIZE_MAP[size], ...style }}
      title={title}
      spin={spin}
      fixedWidth
    />
  );
}
