package service

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// BroadcastService implements preview/send for admin broadcasts. Preview is a
// pure estimate: no audit row, no frame. Send is the only side-effect path.
type BroadcastService struct {
	reach    ReachCounter
	dispatch Dispatcher
	audit    *auditshim.Shim
	now      func() time.Time
}

// NewBroadcastService builds the service.
func NewBroadcastService(reach ReachCounter, dispatch Dispatcher, audit *auditshim.Shim) *BroadcastService {
	return &BroadcastService{reach: reach, dispatch: dispatch, audit: audit, now: time.Now}
}

// SetClock injects a test clock.
func (s *BroadcastService) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Preview estimates reach without producing any side effect.
func (s *BroadcastService) Preview(ctx context.Context, in BroadcastInput) (BroadcastPreview, error) {
	if err := validateBroadcast(in); err != nil {
		return BroadcastPreview{}, err
	}
	var reach int64
	if s.reach != nil {
		n, err := s.reach.CountReachable(ctx, in.ConvIDs)
		if err != nil {
			return BroadcastPreview{}, fmt.Errorf("broadcast: estimate: %w", err)
		}
		reach = n
	}
	return BroadcastPreview{EstimatedReach: reach, ConvCount: len(in.ConvIDs), DryRun: true}, nil
}

// Send injects a 500 ADMIN_BROADCAST frame and audits the action. When
// in.DryRun is true it delegates to Preview and writes nothing.
func (s *BroadcastService) Send(ctx context.Context, actor *identity.Admin, in BroadcastInput) (BroadcastResult, error) {
	if in.DryRun {
		if _, err := s.Preview(ctx, in); err != nil {
			return BroadcastResult{}, err
		}
		return BroadcastResult{Sent: false, FrameKind: 0}, nil
	}
	if err := validateBroadcast(in); err != nil {
		return BroadcastResult{}, err
	}
	estimated := int64(0)
	if s.reach != nil {
		n, err := s.reach.CountReachable(ctx, in.ConvIDs)
		if err == nil {
			estimated = n
		}
	}
	target := bridgeproxy.Target{}
	if len(in.ConvIDs) == 1 {
		target.ConvID = uint64(in.ConvIDs[0])
	}
	payload := map[string]any{
		"text": in.Text, "conv_ids": in.ConvIDs,
		"expires_at": in.ExpiresAt, "by": actorName(actor),
	}
	res, err := auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "broadcast.send", Subject: "broadcast",
		Detail: map[string]any{
			"conv_count": len(in.ConvIDs), "text_len": utf8.RuneCountInString(in.Text),
			"expires_at": in.ExpiresAt, "estimated_reach": estimated,
		},
	}, func(ctx context.Context) (BroadcastResult, error) {
		if _, err := injectJSON(ctx, s.dispatch, kinds.KindAdminBroadcast, target, payload); err != nil {
			return BroadcastResult{}, err
		}
		return BroadcastResult{Sent: true, EstimatedReach: estimated, FrameKind: kinds.KindAdminBroadcast}, nil
	})
	return res, err
}

func validateBroadcast(in BroadcastInput) error {
	n := utf8.RuneCountInString(in.Text)
	if n == 0 {
		return fmt.Errorf("%w: text required", ErrInvalid)
	}
	if n > 4000 {
		return fmt.Errorf("%w: text exceeds 4000 characters", ErrInvalid)
	}
	for _, cid := range in.ConvIDs {
		if cid <= 0 {
			return fmt.Errorf("%w: conv_ids must be positive", ErrInvalid)
		}
	}
	return nil
}
