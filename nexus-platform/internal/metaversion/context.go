// Package metaversion is the draft -> publish layer of a delivered customer workspace
// (specs/063-draft-publish): the request-scoped Caller, the draft and version repositories over
// sys_meta_drafts / sys_meta_versions / sys_meta_version_docs, the per-document-type adapter
// registry and the single class table. It sits below services and middleware (both import it), so
// it never imports either. Nothing outside this package may name sys_meta_drafts (FR-016: the
// engine never reads a draft; structure_test.go pins it).
package metaversion

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"nexus/platform/internal/auth"
)

// Mode is how a document write on a tenant must land.
type Mode int

const (
	// ModeLive writes straight to the live row: the tenant is not delivered yet, or the caller is
	// PublishService applying a publish (WithPublishApply).
	ModeLive Mode = iota
	// ModeDraft writes to sys_meta_drafts: the tenant is delivered and the caller is the builder.
	ModeDraft
	// ModeLiveDirect writes the live row of a delivered tenant for a non-builder (the legacy
	// customer admin); from S7-5 it also records a direct version entry.
	ModeLiveDirect
)

// ErrNoCaller: a document write on a delivered tenant reached the gate with a ctx that never
// passed the DraftCaller middleware. Fail closed (FR-002a): a route that forgot the wiring must
// break loudly, never write the customer's running app.
var ErrNoCaller = errors.New("metaversion: no caller in context (DraftCaller middleware not on this route)")

// ErrCrossTenantDraftWrite: a document write targets a delivered tenant other than the caller's
// token tenant (?targetTenant= style). Drafts are only ever written to the caller's own tenant.
var ErrCrossTenantDraftWrite = errors.New("metaversion: cross-tenant write on a delivered tenant")

// Caller is who is acting on a request, as the DraftCaller middleware resolved it ONCE.
type Caller struct {
	UserID   string
	TenantID string
	// HomeTenantID is the builder's own tenant (ActorTenantID of a workspace session, else the
	// token tenant): the label of a version is read from the caller's own user row there.
	HomeTenantID string
	IsBuilder    bool
	// ReadDraft: this request reads the draft overlay (builder on /api/v1/admin/*, or the
	// X-Nexus-Draft header). WithLiveRead overrides it.
	ReadDraft bool
}

// IsBuilder is THE one definition of "the builder": an admin that is also a platform operator on
// the Subject the Auth stage rebuilt from the live user row (or the workspace-session resolver).
// A preview token has IsAdmin=false pinned, API keys and the non-live Auth stage never set
// IsPlatformOperator, and the flag is json:"-" so no token can claim it.
func IsBuilder(sub auth.Subject) bool { return sub.IsAdmin && sub.IsPlatformOperator }

type ctxKey int

const (
	callerKey ctxKey = iota
	publishApplyKey
	liveReadKey
	sweepAtKey
	sweepCountKey
)

// WithCaller stores the resolved caller on ctx.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerFrom returns the caller, ok=false when ctx never passed DraftCaller (a read then serves
// the live row; a write on a delivered tenant is refused with ErrNoCaller).
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey).(Caller)
	return c, ok
}

// WithPublishApply marks ctx as PublishService applying a publish: writes go live. Never set from HTTP.
func WithPublishApply(ctx context.Context) context.Context {
	return context.WithValue(ctx, publishApplyKey, true)
}

// IsPublishApply reports WithPublishApply.
func IsPublishApply(ctx context.Context) bool {
	v, _ := ctx.Value(publishApplyKey).(bool)
	return v
}

// WithLiveRead forces every read below it to the live row whatever the route or header says —
// pack export / diff / install planning must never read a draft (FR-044).
func WithLiveRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, liveReadKey, true)
}

// IsLiveRead reports WithLiveRead.
func IsLiveRead(ctx context.Context) bool {
	v, _ := ctx.Value(liveReadKey).(bool)
	return v
}

// WithSweepAt carries the soft-delete sweep instant of a publish's deferred schema deletes (S7-9).
func WithSweepAt(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, sweepAtKey, at)
}

// SweepAtFrom returns the instant set by WithSweepAt.
func SweepAtFrom(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(sweepAtKey).(time.Time)
	return at, ok
}

// SweepCount counts the rows a deferred table delete moved to the trash; the entity service adds to it
// so PublishService can record "sweptCount" without the service signature carrying a number.
type SweepCount struct{ N atomic.Int64 }

// WithSweepCount returns ctx carrying a fresh counter, and the counter.
func WithSweepCount(ctx context.Context) (context.Context, *SweepCount) {
	c := &SweepCount{}
	return context.WithValue(ctx, sweepCountKey, c), c
}

// AddSweepCount adds n to the counter on ctx, if any.
func AddSweepCount(ctx context.Context, n int64) {
	if c, ok := ctx.Value(sweepCountKey).(*SweepCount); ok {
		c.N.Add(n)
	}
}

// warningSink collects non-fatal notices of one request (a direct version entry that failed to
// write): the change itself stays applied, the response says so (FR-033).
type warningSink struct {
	mu   sync.Mutex
	msgs []string
}

const warnKey ctxKey = sweepCountKey + 1

// WithWarnings installs a fresh sink; DraftCaller does it once per request.
func WithWarnings(ctx context.Context) context.Context {
	return context.WithValue(ctx, warnKey, &warningSink{})
}

// AddWarning records a notice; a ctx without a sink drops it (the caller already logged the cause).
func AddWarning(ctx context.Context, msg string) {
	if s, ok := ctx.Value(warnKey).(*warningSink); ok {
		s.mu.Lock()
		s.msgs = append(s.msgs, msg)
		s.mu.Unlock()
	}
}

// Warnings returns the notices collected so far on this request.
func Warnings(ctx context.Context) []string {
	s, ok := ctx.Value(warnKey).(*warningSink)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}
