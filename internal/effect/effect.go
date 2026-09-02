// Package effect owns the durable effect journal protocol shared by
// coordinator policies.
package effect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// PendingEffectStore is the optional durable journal used to make external
// mutations recoverable across a coordinator process boundary.
type PendingEffectStore interface {
	PendingEffect(context.Context, string) (*store.PendingEffect, error)
	SavePendingEffect(context.Context, store.PendingEffect) error
	ClearPendingEffect(context.Context, string, string) error
}

// PendingEffectAbandoner is the store-facing capability for explicitly
// discarding one ambiguous effect after human review.
type PendingEffectAbandoner interface {
	AbandonPendingEffect(context.Context, string, string, string) error
}

var _ PendingEffectStore = (*store.Store)(nil)
var _ PendingEffectAbandoner = (*store.Store)(nil)

// ReplayRequest carries the opaque store value needed by an effect handler and
// the durable effect being replayed. The kernel does not interpret the store
// or the payload.
type ReplayRequest struct {
	// Store is the caller's operational-store value.
	Store any
	// Effect is the durable effect selected for replay.
	Effect store.PendingEffect
}

// ReplayHandler is the adapter at the replay seam. Effect-specific policy
// implements this interface and remains outside the kernel.
type ReplayHandler interface {
	Replay(context.Context, ReplayRequest) (store.Run, error)
}

// Dispatcher routes each pending-effect kind to its registered replay
// handler.
type Dispatcher struct {
	handlers map[store.PendingEffectKind]ReplayHandler
}

// NewDispatcher creates an empty replay dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{handlers: make(map[store.PendingEffectKind]ReplayHandler)}
}

// Register associates one pending-effect kind with its replay handler.
func (d *Dispatcher) Register(kind store.PendingEffectKind, handler ReplayHandler) error {
	if d == nil {
		return errors.New("effect dispatcher is required")
	}
	if strings.TrimSpace(string(kind)) == "" {
		return errors.New("pending effect kind is required")
	}
	if handler == nil {
		return errors.New("pending effect replay handler is required")
	}
	if d.handlers == nil {
		d.handlers = make(map[store.PendingEffectKind]ReplayHandler)
	}
	if _, exists := d.handlers[kind]; exists {
		return fmt.Errorf("pending effect kind %q already has a replay handler", kind)
	}
	d.handlers[kind] = handler
	return nil
}

// Replay dispatches one durable effect to the handler registered for its kind.
func (d *Dispatcher) Replay(ctx context.Context, runStore any, pending store.PendingEffect) (store.Run, error) {
	if d != nil {
		if handler, exists := d.handlers[pending.Kind]; exists {
			return handler.Replay(ctx, ReplayRequest{Store: runStore, Effect: pending})
		}
	}
	return store.Run{}, &UnknownKindError{Kind: pending.Kind}
}

// UnknownKindError reports that no replay handler is registered for a pending
// effect kind.
type UnknownKindError struct {
	// Kind is the unregistered pending-effect kind.
	Kind store.PendingEffectKind
}

// Error returns the stable unsupported-kind error used by the coordinator.
func (e *UnknownKindError) Error() string {
	if e == nil {
		return "unsupported pending effect kind \"\""
	}
	return fmt.Sprintf("unsupported pending effect kind %q", e.Kind)
}

// PendingEffectID derives the stable identity used to recognize one semantic
// effect after a coordinator restart. The NUL-delimited input is part of the
// persisted protocol and must remain byte-identical across upgrades.
func PendingEffectID(runID string, kind store.PendingEffectKind, identity string) string {
	digest := sha256.Sum256([]byte(runID + "\x00" + string(kind) + "\x00" + identity))
	return string(kind) + ":" + hex.EncodeToString(digest[:])
}

// NewPendingEffect encodes one replay intent and assigns the supplied
// coordinator time to both journal timestamps.
func NewPendingEffect(now time.Time, runID string, kind store.PendingEffectKind, identity string, payload any) (store.PendingEffect, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return store.PendingEffect{}, fmt.Errorf("encode %s effect: %w", kind, err)
	}
	if len(data) == 0 || !json.Valid(data) {
		return store.PendingEffect{}, errors.New("pending effect payload is not valid JSON")
	}
	when := now.UTC()
	return store.PendingEffect{
		RunID:     runID,
		ID:        PendingEffectID(runID, kind, identity),
		Kind:      kind,
		Payload:   string(data),
		CreatedAt: when,
		UpdatedAt: when,
	}, nil
}

// DecodePendingEffect decodes one bounded replay payload and reports malformed
// journal data as an infrastructure discrepancy rather than a workflow result.
func DecodePendingEffect(pending store.PendingEffect, destination any) error {
	if strings.TrimSpace(pending.Payload) == "" {
		return errors.New("pending effect payload is empty")
	}
	if err := json.Unmarshal([]byte(pending.Payload), destination); err != nil {
		return fmt.Errorf("decode %s effect: %w", pending.Kind, err)
	}
	return nil
}

// WithPendingEffect reserves one external mutation before applying it and
// completes the reservation only after the mutation succeeds. A store without
// the optional journal retains the legacy direct-execution behavior.
func WithPendingEffect(ctx context.Context, runStore any, pending store.PendingEffect, apply func() error) error {
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return apply()
	}
	if err := journal.SavePendingEffect(ctx, pending); err != nil {
		return fmt.Errorf("reserve %s effect: %w", pending.Kind, err)
	}
	if err := apply(); err != nil {
		return err
	}
	if err := journal.ClearPendingEffect(ctx, pending.RunID, pending.ID); err != nil {
		return fmt.Errorf("complete %s effect: %w", pending.Kind, err)
	}
	return nil
}
