/** 回复/编辑中的临时状态（跨组件共享）。 */

import { create } from 'zustand';
import type { ChatMessage } from '@/core/domain/types';

interface ReplyState {
  replyTo: ChatMessage | null;
  setReplyTo: (message: ChatMessage | null) => void;
}

export const useReplyStore = create<ReplyState>((set) => ({
  replyTo: null,
  setReplyTo: (message) => set({ replyTo: message }),
}));
