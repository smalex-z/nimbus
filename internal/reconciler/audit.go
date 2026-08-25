package reconciler

import "context"

// AuditReporter is the slice of audit.Service the reconciler emits
// to. Defined here at the consumer per the "accept interfaces" idiom;
// audit.Service satisfies it directly. Nil-safe — when no reporter is
// installed, the reconciler still functions and logs to stdout.
//
// Why we don't import audit directly: keeps the reconciler package
// free of an audit → reconciler edge, which would otherwise force
// `audit` to import `reconciler` if we ever wanted typed cross-
// package events. The interface inversion costs three lines and
// removes the dependency entirely.
type AuditReporter interface {
	Record(ctx context.Context, evt AuditEvent)
}

// AuditEvent mirrors audit.Event so the reconciler can build one
// without importing the audit package. Field meanings are identical —
// see internal/audit/service.go for the canonical docs.
type AuditEvent struct {
	Action      string
	TargetType  string
	TargetID    string
	TargetLabel string
	Details     any
	Success     bool
	ErrorMsg    string
}

// SetAudit installs the audit reporter. Optional — when nil the
// reconciler skips emit calls but still functions. Wired from main.go
// once the audit.Service is constructed.
func (r *Reconciler) SetAudit(rep AuditReporter) {
	r.audit = rep
}

// emit is a nil-safe helper so the call sites don't repeat the guard.
func (r *Reconciler) emit(ctx context.Context, evt AuditEvent) {
	if r.audit == nil {
		return
	}
	r.audit.Record(ctx, evt)
}
