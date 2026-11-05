package envspec

import "errors"

// Sentinel errors matched with errors.Is at the public boundary. Internal
// packages keep their own finer-grained causes.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrNotReady = errors.New("not ready")
	// ErrConflict rejects a stale-revision transition, a request key reused
	// with a different request, and a stale handle used after reassignment.
	ErrConflict = errors.New("conflict")

	// ErrBackupRejected marks a backup the transport or target declined at
	// preflight. It is a target/transport condition, distinct from a
	// backup-manager or tooling failure.
	ErrBackupRejected = errors.New("backup transport rejected package at preflight")
	// ErrBackupRecipientEmpty marks a backup for which the manager reported
	// success but the selected transport retained no package objects.
	ErrBackupRecipientEmpty = errors.New("local backup transport retained no recipient objects")
)
