package environments

import "github.com/craggy-io/environment-services/internal/core"

// Credentials is the port to the application's credential store, set as
// Config.Credentials. A holder is "env:<environment id>" and a use is one
// of envspec's Use constants (UseAndroidAccount, UseLogin, UseInject),
// which the application declares to its store.
//
//	Hold(ctx, CredentialRequest) (CredentialLease, error)
//	Redeem(ctx, lease, holder string) (HeldCredential, error)
//	RedeemOnce(ctx, lease, holder, field string) (HeldCredential, error)
//	RedeemHolder(ctx, holder string) ([]HeldCredential, error)
//	Drop(ctx, holders []string) error
//
// Each method is called with the context of the work it is part of. When
// that context belongs to no call the application can act under, return
// ErrNoCall: the manager defers the work to Manager.SyncCredentials.
// Otherwise wrap envspec.ErrNotFound, ErrCredentialUnavailable or
// ErrCredentialDenied so callers are told which it was.
type Credentials = core.Credentials

// CredentialRequest asks the port for a lease.
type CredentialRequest = core.CredentialRequest

// CredentialLease is a lease as anyone may see it: no secret.
type CredentialLease = core.CredentialLease

// HeldCredential is a lease with the secret behind it, by field. The
// manager delivers it and teaches its redactor the values; it never
// stores, logs or returns them.
type HeldCredential = core.HeldCredential

var (
	// ErrNoCall is what a Credentials method returns when its context
	// belongs to no call it can act under.
	ErrNoCall = core.ErrNoCall
	// ErrCredentialUnavailable marks a lease the store's limits refuse
	// for now.
	ErrCredentialUnavailable = core.ErrCredentialUnavailable
	// ErrCredentialDenied marks a credential that may not be put to the
	// use asked for.
	ErrCredentialDenied = core.ErrCredentialDenied
)
