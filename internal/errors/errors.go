package errors

import "fmt"

type NotFoundError struct {
	Resource string
	ID       string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s with id %s not found", e.Resource, e.ID)
}

type ConflictError struct {
	Message string
}

func (e *ConflictError) Error() string {
	return e.Message
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s - %s", e.Field, e.Message)
}

// OnlineMigrationFailedError signals that a live (online=1) Proxmox VM
// migration was rejected by the hypervisor. Reason carries the upstream
// error string so the operator can see the actual cause (no shared
// storage, snapshot present, local CD/DVD, etc.) and decide whether
// retrying as an offline migration would help.
//
// The cluster.MigrateVM handler maps this to a 409 with a structured
// payload (`code: "online_migration_failed"`) so the SPA can render the
// "continue offline" confirmation rather than treating it as a generic
// failure.
type OnlineMigrationFailedError struct {
	Reason string
}

func (e *OnlineMigrationFailedError) Error() string {
	return fmt.Sprintf("online migration failed: %s", e.Reason)
}
