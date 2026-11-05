// Package xfer moves file trees between envspec.Source/Sink and
// environments: streaming tar, manifests, delta sync, and the
// in-environment helper protocol. It imports envspec only — never
// storage and never a driver. The in-memory Source/Sink live in envspec
// (MemSource/MemSink); the delta-sink extensions live here.
package xfer

import (
	"context"
	"io"
	"strconv"

	"github.com/craggy-io/environment-services/envspec"
)

// Options tune a transfer.
type Options struct {
	Mirror   bool // delete destination entries not in the source (push)
	Delta    bool // send only differences vs the destination manifest
	Workers  int  // receive fan-out for ConcurrentSinks; default 8
	Progress func(envspec.Progress)
}

func (o Options) report(s envspec.TransferStats, op string) {
	if o.Progress != nil {
		o.Progress(envspec.Progress{Op: op, Stage: op, Done: s.Bytes, Unit: "bytes",
			Message: "files: " + strconv.FormatInt(s.Files, 10)})
	}
}

// DigestSink is an optional Sink extension for delta pull: entries the
// sink already holds by digest are recorded without moving bytes.
type DigestSink interface {
	Has(digest string) bool
	AddDigest(path string, mode int64, digest string, size int64) error
}

// ConcurrentSink marks a Sink whose Add is safe for concurrent use.
type ConcurrentSink interface {
	envspec.Sink
	Concurrent() bool
}

// entryIsSymlink reports whether e carries the symlink file-type bits
// (git convention: Mode 0o120000, target as content).
func entryIsSymlink(e envspec.Entry) bool { return e.Mode&0o170000 == 0o120000 }

// ExecFunc runs argv in an environment with streamed stdio. Core adapts
// an adapter's exec port to it; returning a wait func avoids importing
// the lifecycle packages here.
type ExecFunc func(ctx context.Context, argv []string, stdin io.Reader) (stdout io.Reader, wait func() (int, error), err error)
