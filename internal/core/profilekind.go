package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/bundle/compile"
	"github.com/craggy-io/foundation-kit/kind"
)

// ProfileKind is an environment profile: one envspec.Profile per JSON
// file, whose name must equal the contribution id. Profiles are looked up
// by name, so the kind is global: two owners defining the same name is a
// bundle.id_conflict unless a bundle override settles it.
var ProfileKind = kind.New[envspec.Profile]("environments.profile", 1, kind.Global())

const (
	// stockProvider owns the built-in profiles (StockProfiles).
	stockProvider = "craggy.environments.stock"
	// configProvider owns Config.Profiles. It is not an override layer,
	// so a Config profile named like a stock or user profile is an error.
	configProvider = "craggy.environments.config"
	// userProfilesBundle is the metadata.id the <Dir>/profiles bundle must
	// declare. That bundle overrides the stock provider: a profile with a
	// stock name replaces the stock one, and new names are added.
	userProfilesBundle = "craggy.environments.user"
)

// compileProfile decodes one profile file and checks it as
// RegisterProfiles would, so problems carry the file's path.
func compileProfile(ctx context.Context, c bundle.Contribution) (bundle.Compiled[envspec.Profile], []bundle.Diagnostic) {
	out, diags := compile.JSON[envspec.Profile]()(ctx, c)
	if len(diags) > 0 {
		return out, diags
	}
	fail := func(msg string) (bundle.Compiled[envspec.Profile], []bundle.Diagnostic) {
		return bundle.Compiled[envspec.Profile]{}, []bundle.Diagnostic{{
			Code: bundle.CodeCompileFailed, Severity: bundle.SeverityError,
			Bundle: c.Bundle.ID, Contribution: c.Ref.ID, Path: c.Path, Message: msg,
		}}
	}
	if out.Value.Name != c.Ref.ID {
		return fail(fmt.Sprintf("profile name %q must equal the contribution id %q", out.Value.Name, c.Ref.ID))
	}
	if err := validateProfile(out.Value); err != nil {
		return fail(err.Error())
	}
	return out, nil
}

func profileProvider(id string, ps []envspec.Profile) kind.Provider {
	p := kind.Provider{ID: id, Version: "1"}
	for _, pr := range ps {
		p.Builtins = append(p.Builtins, kind.Builtin(ProfileKind, pr.Name, pr))
	}
	return p
}

// loadProfiles loads the stock profiles, the user profiles bundle at
// <dir>/profiles (if that folder exists) and the Config profiles through
// one bundle.Load. A missing folder means no user profiles; a folder
// without bundle.yaml is an error, as is any duplicate name that no
// override settles.
func loadProfiles(ctx context.Context, dir string, config []envspec.Profile) ([]envspec.Profile, error) {
	opts := bundle.Options{
		Compilers: []bundle.AnyCompiler{bundle.Compile(ProfileKind, compileProfile)},
		Providers: []kind.Provider{profileProvider(stockProvider, StockProfiles())},
	}
	if len(config) > 0 {
		opts.Providers = append(opts.Providers, profileProvider(configProvider, config))
	}
	// A missing or empty folder means no user profiles. Earlier versions created
	// an empty profiles folder in every state directory.
	userDir := filepath.Join(dir, "profiles")
	switch entries, err := os.ReadDir(userDir); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case len(entries) == 0:
	default:
		opts.Sources = []bundle.Source{bundle.Dir(userDir)}
		opts.BundleOverrides = []bundle.BundleOverride{{Target: stockProvider, Replacement: userProfilesBundle}}
	}
	cat, err := bundle.Load(ctx, opts)
	if err != nil {
		if len(opts.Sources) > 0 {
			return nil, fmt.Errorf("core: load profiles (%s must be a bundle: a bundle.yaml with metadata.id %s, one profile per file): %w",
				userDir, userProfilesBundle, err)
		}
		return nil, fmt.Errorf("core: load profiles: %w", err)
	}
	var out []envspec.Profile
	for _, d := range bundle.All(cat, ProfileKind) {
		out = append(out, d.Value)
	}
	return out, nil
}
