// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/presets"
)

// ErrInvalidPreset marks a PutPreset failure that is the caller's fault — a
// preset that fails validation — as opposed to a store failure. The manager
// wraps it; presetError maps it to CodeInvalidArgument instead of
// CodeInternal, so a bad preset reads as a bad request.
var ErrInvalidPreset = errors.New("invalid preset")

// PresetManager is the narrow slice of the daemon needed to manage the
// caller's own presets. The owner is resolved INSIDE the implementation from
// the request context -- no method takes one, the same rule as
// PymoduleManager: a user credential reaches its own bucket, the anonymous
// unix-socket caller shares the daemon's unattributed bucket.
type PresetManager interface {
	// ListPresets lists the caller's latest live preset whose name starts
	// with prefix ("" = all).
	ListPresets(ctx context.Context, prefix string) ([]presets.Record, error)
	// GetPreset returns the latest live row (one element), or with history
	// every row newest first, deleted ones included.
	GetPreset(ctx context.Context, name string, history bool) ([]presets.Record, error)
	// PutPreset validates and stores spec as a new version.
	// Validation failures must be wrapped with ErrInvalidPreset (mapped to
	// CodeInvalidArgument); store errors must not be.
	PutPreset(ctx context.Context, spec presets.Spec) (presets.Record, error)
	// DeletePreset stamps deleted_at on every live row for name.
	DeletePreset(ctx context.Context, name string) error
}

// SetPresetManager attaches the presets backend. A nil manager is refused
// rather than stored (see SetPymoduleManager): storing &m for a nil
// interface would defeat presetManager's Unavailable path and nil-panic the
// first handler call instead.
func (s *Server) SetPresetManager(m PresetManager) {
	if m == nil {
		return
	}
	s.presets.Store(&m)
}

func (s *Server) presetManager() (PresetManager, error) {
	p := s.presets.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("presets backend not yet wired"))
	}
	return *p, nil
}

// presetError maps a manager failure onto a Connect code: an unknown preset
// is CodeNotFound, a preset that failed validation (ErrInvalidPreset) is a
// CodeInvalidArgument, and everything else — a store failure — is
// CodeInternal rather than something that reads as the caller's fault.
func presetError(err error) error {
	if errors.Is(err, presets.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	if errors.Is(err, ErrInvalidPreset) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func (s *Server) ListPresets(
	ctx context.Context, req *connect.Request[rafikiv1.ListPresetsRequest],
) (*connect.Response[rafikiv1.ListPresetsResponse], error) {
	m, err := s.presetManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.ListPresets(ctx, req.Msg.GetPrefix())
	if err != nil {
		return nil, presetError(err)
	}
	return connect.NewResponse(&rafikiv1.ListPresetsResponse{Rows: presetRows(rows)}), nil
}

func (s *Server) GetPreset(
	ctx context.Context, req *connect.Request[rafikiv1.GetPresetRequest],
) (*connect.Response[rafikiv1.GetPresetResponse], error) {
	m, err := s.presetManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	rows, err := m.GetPreset(ctx, req.Msg.GetName(), req.Msg.GetHistory())
	if err != nil {
		return nil, presetError(err)
	}
	return connect.NewResponse(&rafikiv1.GetPresetResponse{Rows: presetRows(rows)}), nil
}

func (s *Server) PutPreset(
	ctx context.Context, req *connect.Request[rafikiv1.PutPresetRequest],
) (*connect.Response[rafikiv1.PutPresetResponse], error) {
	m, err := s.presetManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetPreset() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("preset is required"))
	}
	if req.Msg.GetPreset().GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	rec, err := m.PutPreset(ctx, presets.SpecOf(presets.FromProto(req.Msg.GetPreset())))
	if err != nil {
		return nil, presetError(err)
	}
	return connect.NewResponse(&rafikiv1.PutPresetResponse{Preset: presets.ToProto(rec)}), nil
}

func (s *Server) DeletePreset(
	ctx context.Context, req *connect.Request[rafikiv1.DeletePresetRequest],
) (*connect.Response[rafikiv1.DeletePresetResponse], error) {
	m, err := s.presetManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if err := m.DeletePreset(ctx, req.Msg.GetName()); err != nil {
		return nil, presetError(err)
	}
	return connect.NewResponse(&rafikiv1.DeletePresetResponse{}), nil
}

// presetRows converts a manager's records for a repeated-PresetRow response.
func presetRows(rows []presets.Record) []*rafikiv1.PresetRow {
	out := make([]*rafikiv1.PresetRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, presets.ToProto(r))
	}
	return out
}
