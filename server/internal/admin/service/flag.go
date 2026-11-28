package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/yingo-server/UFOT/server/internal/admin/auditshim"
	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// flagKeyRE keeps flag keys boring: lowercase dot/dash/underscore names. It
// also prevents injecting arbitrary SQL-ish strings into logging/UI.
var flagKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

// FlagService exposes feature flags as policy data. Writes go to the policy
// store and are also fanned out as 971 FLAG_SET frames (flag chain = policy).
type FlagService struct {
	store    FlagStore
	dispatch Dispatcher
	audit    *auditshim.Shim
}

// NewFlagService builds the service.
func NewFlagService(store FlagStore, dispatch Dispatcher, audit *auditshim.Shim) *FlagService {
	return &FlagService{store: store, dispatch: dispatch, audit: audit}
}

// List returns all flags sorted by key.
func (s *FlagService) List(ctx context.Context) ([]Flag, error) {
	if s.store == nil {
		return nil, ErrNotConfigured
	}
	flags, err := s.store.ListFlags(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i].Key < flags[j].Key })
	return flags, nil
}

// Set validates and writes one flag, then injects a 971 frame. Value must be
// valid JSON (policy documents are JSON values).
func (s *FlagService) Set(ctx context.Context, actor *identity.Admin, key, value string) (*Flag, error) {
	if s.store == nil {
		return nil, ErrNotConfigured
	}
	if !flagKeyRE.MatchString(key) {
		return nil, fmt.Errorf("%w: invalid flag key", ErrInvalid)
	}
	if len(value) > 4096 || !json.Valid([]byte(value)) {
		return nil, fmt.Errorf("%w: value must be valid JSON (<=4096 bytes)", ErrInvalid)
	}
	f, err := auditshim.Do(ctx, s.audit, auditshim.Meta{
		Actor: actorName(actor), Action: "flag.set", Subject: "flag:" + key,
		Detail: map[string]any{"key": key, "value_len": len(value)},
	}, func(ctx context.Context) (*Flag, error) {
		if err := s.store.SetFlag(ctx, key, value); err != nil {
			return nil, err
		}
		if _, err := injectJSON(ctx, s.dispatch, kinds.KindFlagSet, bridgeproxy.Target{}, map[string]any{
			"key": key, "value": value, "by": actorName(actor),
		}); err != nil {
			return nil, err
		}
		return &Flag{Key: key, Value: value}, nil
	})
	return f, err
}

// PolicyStore is the narrow policy-table source the admin plane needs.
// data/store.SysStore satisfies it; tests can fake just these two methods.
type PolicyStore interface {
	AllFlags(ctx context.Context) ([]store.Flag, error)
	UpsertFlag(ctx context.Context, f *store.Flag) error
}

// SysFlagStore adapts a PolicyStore to the FlagStore port.
type SysFlagStore struct {
	Sys PolicyStore
}

// ListFlags implements FlagStore.
func (s SysFlagStore) ListFlags(ctx context.Context) ([]Flag, error) {
	if s.Sys == nil {
		return nil, ErrNotConfigured
	}
	rows, err := s.Sys.AllFlags(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Flag, 0, len(rows))
	for _, r := range rows {
		out = append(out, Flag{Key: r.Key, Value: r.Value, Version: r.Version})
	}
	return out, nil
}

// SetFlag implements FlagStore.
func (s SysFlagStore) SetFlag(ctx context.Context, key, value string) error {
	if s.Sys == nil {
		return ErrNotConfigured
	}
	return s.Sys.UpsertFlag(ctx, &store.Flag{Key: key, Value: value, Version: 1})
}
