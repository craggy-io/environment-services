package envcap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/craggy-io/environment-services/envspec"
)

// Operation is the identity of one artifact-producing operation.
// ID is always set: for lifecycle capabilities it is the durable
// lifecycle operation; for artifact runs minted here it is the
// publication identity the bridge records. JobID is the registry
// job (registry.JobIDFrom) — never the owner's job — and Parent is
// the parent operation id from WithParentOperation. Env, Generation
// and LeaseEpoch fence the run to one environment incarnation.
type Operation struct {
	ID         string
	Capability string // exact name@version
	JobID      string
	Parent     string
	Env        string
	Generation int64
	LeaseEpoch int64
	Inputs     []string
}

// Evidence recorded against an artifact operation.
type Evidence struct {
	Environment string
	Image       string
	Collector   string
	RuleCode    string
	Range       string
}

// Ref is one published artifact output.
type Ref struct {
	ID string `json:"id"`
}

// ArtifactRun is one operation's read/write surface. Outputs are
// staged, then kept: Stage builds an output under a name without
// publishing it; Keep publishes a staged output durably, so output
// produced before a later failure survives. Nothing staged but not
// kept is published.
type ArtifactRun interface {
	Source(ctx context.Context, artifactID string) (envspec.Source, error)
	Stage(ctx context.Context, name string, build func(envspec.Sink) error) error
	Keep(name string) error
	Record(ctx context.Context, evidence Evidence) error
}

// Artifacts is the handles-only bridge the host implements once over
// its artifact store. Run executes fn against a fresh run and
// returns the refs of every kept output by name. When fn (or the
// bridge) fails after some outputs were kept, Run returns those
// refs together with an error wrapping *PartialOutputs: partial
// output is kept deliberately, never silently dropped.
type Artifacts interface {
	Run(ctx context.Context, op Operation, fn func(context.Context, ArtifactRun) error) (map[string]Ref, error)
}

// PartialOutputs reports the outputs that were kept before a run
// failed. Recover it with PartialOutputsOf.
type PartialOutputs struct {
	Outputs map[string]Ref
	Err     error
}

func (e *PartialOutputs) Error() string {
	return fmt.Sprintf("partial outputs kept (%d): %v", len(e.Outputs), e.Err)
}

func (e *PartialOutputs) Unwrap() error { return e.Err }

// PartialOutputsOf extracts kept outputs from a failed run's error.
func PartialOutputsOf(err error) (map[string]Ref, bool) {
	var p *PartialOutputs
	if errors.As(err, &p) {
		return p.Outputs, true
	}
	return nil, false
}

type parentOpKey struct{}

// WithParentOperation carries the parent durable operation id for
// operations started under ctx (a host running capabilities inside
// a larger operation stamps it; handlers copy it to Operation.Parent).
func WithParentOperation(ctx context.Context, opID string) context.Context {
	return context.WithValue(ctx, parentOpKey{}, opID)
}

func parentFrom(ctx context.Context) string {
	id, _ := ctx.Value(parentOpKey{}).(string)
	return id
}

// ModesManifestName is the metadata file an artifact bridge stores
// alongside an output tree to preserve what a plain file walk
// loses: directory entries, symlink targets (carried as content,
// git convention) and permission bits (including exec). The format
// is this package's EncodeModes/DecodeModes, so bridge and readers
// cannot drift.
const ModesManifestName = "craggy.envspec.modes"

// ModeEntry is one row of the modes manifest.
type ModeEntry struct {
	Path string `json:"path"`
	Mode int64  `json:"mode"`
	Size int64  `json:"size"`
}

// EncodeModes renders entries (sorted, as Sources yield them) as the
// manifest body.
func EncodeModes(entries []envspec.Entry) ([]byte, error) {
	rows := make([]ModeEntry, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, ModeEntry{Path: e.Path, Mode: e.Mode, Size: e.Size})
	}
	return json.Marshal(rows)
}

// DecodeModes parses a manifest body back to entries.
func DecodeModes(b []byte) ([]envspec.Entry, error) {
	var rows []ModeEntry
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, err
	}
	out := make([]envspec.Entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, envspec.Entry{Path: r.Path, Mode: r.Mode, Size: r.Size})
	}
	return out, nil
}

// copyTree copies every entry of src into sink, preserving modes.
// Directories are added as explicit entries (empty body), symlinks
// as their target content under the symlink mode — nothing is
// skipped, so a tree survives the round trip intact. It stops at
// the first error; what was already added stays added, which is
// what makes Stage safe to Keep after a partial copy failure.
func copyTree(ctx context.Context, src envspec.Source, sink envspec.Sink) error {
	for en, err := range src.Entries(ctx) {
		if err != nil {
			return err
		}
		if en.IsDir() {
			if err := sink.Add(en.Path, en.Mode, strings.NewReader("")); err != nil {
				return err
			}
			continue
		}
		rc, err := src.Open(ctx, en)
		if err != nil {
			return err
		}
		err = sink.Add(en.Path, en.Mode, rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// stageTree stages src as output name, prefixing the modes manifest
// so the bridge can restore dirs/symlinks/modes on the way out.
func stageTree(ctx context.Context, run ArtifactRun, name string, src envspec.Source) error {
	return run.Stage(ctx, name, func(sink envspec.Sink) error {
		man, err := manifestOf(ctx, src)
		if err != nil {
			return err
		}
		body, err := EncodeModes(man)
		if err != nil {
			return err
		}
		if err := sink.Add(ModesManifestName, 0o100644, strings.NewReader(string(body))); err != nil {
			return err
		}
		return copyTree(ctx, src, sink)
	})
}

func manifestOf(ctx context.Context, src envspec.Source) ([]envspec.Entry, error) {
	var out []envspec.Entry
	for en, err := range src.Entries(ctx) {
		if err != nil {
			return nil, err
		}
		out = append(out, en)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// firstFile returns the first non-directory entry's bytes.
func firstFile(ctx context.Context, src envspec.Source) ([]byte, error) {
	for en, err := range src.Entries(ctx) {
		if err != nil {
			return nil, err
		}
		if en.IsDir() {
			continue
		}
		rc, err := src.Open(ctx, en)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return nil, errors.New("artifact has no files")
}
