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

// effectApplier performs the effect-specific external mutation wrapped by the
// journal protocol.
type effectApplier interface {
	Apply() error
}

var _ PendingEffectStore = (*store.Store)(nil)
var _ PendingEffectAbandoner = (*store.Store)(nil)

// replayRequest carries the opaque store value needed by an effect handler and
// the durable effect being replayed. The kernel does not interpret the store
// or the payload.
type replayRequest struct {
	// Store is the caller's operational-store value.
	Store any
	// Effect is the durable effect selected for replay.
	Effect store.PendingEffect
}

// replayHandler is the adapter at the replay seam.
type replayHandler interface {
	Replay(context.Context, replayRequest) (store.Run, error)
}

// handlerRegistration binds one kind's typed apply implementation to its
// replay implementation. The apply value is recovered by the journal's typed
// operation, so the registry does not merge the kinds into a generic effect.
type handlerRegistration struct {
	apply  any
	replay replayHandler
}

// dispatcher routes each pending-effect kind through its one registered apply
// implementation and one registered replay implementation.
type dispatcher struct {
	handlers map[store.PendingEffectKind]handlerRegistration
}

// newDispatcher creates an empty apply/replay dispatcher.
func newDispatcher() *dispatcher {
	return &dispatcher{handlers: make(map[store.PendingEffectKind]handlerRegistration)}
}

// register associates one pending-effect kind with exactly one apply and one
// replay implementation.
func (d *dispatcher) register(kind store.PendingEffectKind, apply any, replay replayHandler) error {
	if d == nil {
		return errors.New("effect dispatcher is required")
	}
	if strings.TrimSpace(string(kind)) == "" {
		return errors.New("pending effect kind is required")
	}
	if apply == nil {
		return errors.New("pending effect apply handler is required")
	}
	if replay == nil {
		return errors.New("pending effect replay handler is required")
	}
	if d.handlers == nil {
		d.handlers = make(map[store.PendingEffectKind]handlerRegistration)
	}
	if _, exists := d.handlers[kind]; exists {
		return fmt.Errorf("pending effect kind %q already has handlers", kind)
	}
	d.handlers[kind] = handlerRegistration{apply: apply, replay: replay}
	return nil
}

// replay dispatches one durable effect to the handler registered for its kind.
func (d *dispatcher) replay(ctx context.Context, runStore any, pending store.PendingEffect) (store.Run, error) {
	if d != nil {
		if handlers, exists := d.handlers[pending.Kind]; exists {
			return handlers.replay.Replay(ctx, replayRequest{Store: runStore, Effect: pending})
		}
	}
	return store.Run{}, &UnknownKindError{Kind: pending.Kind}
}

// mustApplyHandler returns the typed apply implementation registered for a
// package-owned kind. A mismatch is a construction bug and therefore panics at
// the same boundary as a duplicate registration.
func mustApplyHandler[T any](d *dispatcher, kind store.PendingEffectKind) T {
	if d == nil {
		panic("effect dispatcher is required")
	}
	handlers, ok := d.handlers[kind]
	if !ok {
		panic(fmt.Sprintf("pending effect kind %q has no apply handler", kind))
	}
	handler, ok := handlers.apply.(T)
	if !ok {
		panic(fmt.Sprintf("pending effect kind %q has the wrong apply handler", kind))
	}
	return handler
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

// pendingEffectID derives the stable identity used to recognize one semantic
// effect after a coordinator restart. The NUL-delimited input is part of the
// persisted protocol and must remain byte-identical across upgrades.
func pendingEffectID(runID string, kind store.PendingEffectKind, identity string) string {
	digest := sha256.Sum256([]byte(runID + "\x00" + string(kind) + "\x00" + identity))
	return string(kind) + ":" + hex.EncodeToString(digest[:])
}

// newPendingEffect encodes one replay intent and assigns the supplied
// coordinator time to both journal timestamps.
func newPendingEffect(now time.Time, runID string, kind store.PendingEffectKind, identity string, payload any) (store.PendingEffect, error) {
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
		ID:        pendingEffectID(runID, kind, identity),
		Kind:      kind,
		Payload:   string(data),
		CreatedAt: when,
		UpdatedAt: when,
	}, nil
}

// decodePendingEffect decodes one bounded replay payload and reports malformed
// journal data as an infrastructure discrepancy rather than a workflow result.
func decodePendingEffect(pending store.PendingEffect, destination any) error {
	if strings.TrimSpace(pending.Payload) == "" {
		return errors.New("pending effect payload is empty")
	}
	if err := json.Unmarshal([]byte(pending.Payload), destination); err != nil {
		return fmt.Errorf("decode %s effect: %w", pending.Kind, err)
	}
	return nil
}

// withPendingEffect reserves one external mutation before applying it and
// completes the reservation only after the mutation succeeds. A store without
// the optional journal retains the legacy direct-execution behavior.
func withPendingEffect(ctx context.Context, runStore any, pending store.PendingEffect, applier effectApplier) error {
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return applier.Apply()
	}
	if err := journal.SavePendingEffect(ctx, pending); err != nil {
		return fmt.Errorf("reserve %s effect: %w", pending.Kind, err)
	}
	if err := applier.Apply(); err != nil {
		return err
	}
	if err := journal.ClearPendingEffect(ctx, pending.RunID, pending.ID); err != nil {
		return fmt.Errorf("complete %s effect: %w", pending.Kind, err)
	}
	return nil
}
