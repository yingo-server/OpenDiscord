package service

import (
	"context"
	"fmt"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// MessageService implements admin message search and recall. Search is a read
// port; recall is a 960 security frame that fans out through the bridge so
// both hot storage and every connected client see it.
type MessageService struct {
	repo     MessageRepository
	dispatch Dispatcher
	audit    *auditshim.Shim
}

// NewMessageService builds the service.
func NewMessageService(repo MessageRepository, dispatch Dispatcher, audit *auditshim.Shim) *MessageService {
	return &MessageService{repo: repo, dispatch: dispatch, audit: audit}
}

// Search returns one page of message hits.
func (s *MessageService) Search(ctx context.Context, actor *identity.Admin, q MessageQuery) (MessagePage, error) {
	if s.repo == nil {
		return MessagePage{}, ErrNotConfigured
	}
	if len(q.Query) > 200 {
		return MessagePage{}, fmt.Errorf("%w: query too long", ErrInvalid)
	}
	q.Limit = normalizeLimit(q.Limit, 50, 200)
	q.Offset = normalizeOffset(q.Offset)
	if q.Query == "" && q.ConvID <= 0 {
		return MessagePage{}, fmt.Errorf("%w: q or conv_id required", ErrInvalid)
	}
	page, err := s.repo.SearchMessages(ctx, q)
	if err != nil {
		return MessagePage{}, mapRepoErr(err)
	}
	page.Limit, page.Offset = q.Limit, q.Offset
	return page, nil
}

// RecallResult is the response of an admin recall.
type RecallResult struct {
	MsgID     int64  `json:"msg_id"`
	Recalled  bool   `json:"recalled"`
	FrameKind uint32 `json:"frame_kind"`
}

// Recall force-recalls a message regardless of the sender's 2-minute window
// (admin power, high-risk rate class).
func (s *MessageService) Recall(ctx context.Context, actor *identity.Admin, msgID int64, reason string) (RecallResult, error) {
	if msgID <= 0 {
		return RecallResult{}, fmt.Errorf("%w: msg_id must be positive", ErrInvalid)
	}
	payload := map[string]any{
		"op": "message.recall", "msg_id": msgID, "reason": reason, "by": actorName(actor),
	}
	return auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "message.recall", Subject: "message:" + itoa(msgID),
		Detail: map[string]any{"reason_len": len(reason)},
	}, func(ctx context.Context) (RecallResult, error) {
		if _, err := injectJSON(ctx, s.dispatch, kinds.KindSecurityRequest,
			bridgeproxy.Target{}, payload); err != nil {
			return RecallResult{}, err
		}
		return RecallResult{MsgID: msgID, Recalled: true, FrameKind: kinds.KindSecurityRequest}, nil
	})
}
