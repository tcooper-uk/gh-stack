package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/stack"
)

// A base ref is a tag or branch that a stack is rebased onto instead of the
// head of its trunk branch, for example a moving "stable" tag that marks the
// last trunk commit to pass CI. Pull requests still target the trunk branch.

// effectiveBaseRef returns the ref to rebase onto in place of the trunk head:
// the --base-ref flag, else the gh-stack.base-ref git config, else "".
func effectiveBaseRef(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return git.ConfiguredBaseRef()
}

// classifyBaseRef resolves a user-supplied base ref to a tag or branch name on
// the remote. Full refs and remote-qualified branches are taken as given; a
// bare name is looked up on the remote and must match exactly one of a tag or
// a branch.
func classifyBaseRef(remote, spec string) (git.RemoteRefKind, string, error) {
	switch {
	case strings.HasPrefix(spec, "refs/tags/"):
		return git.RemoteRefTag, strings.TrimPrefix(spec, "refs/tags/"), nil
	case strings.HasPrefix(spec, "refs/heads/"):
		return git.RemoteRefBranch, strings.TrimPrefix(spec, "refs/heads/"), nil
	case strings.HasPrefix(spec, "refs/"):
		return git.RemoteRefNone, "", fmt.Errorf("unsupported base ref %q: use a tag or a branch", spec)
	case strings.HasPrefix(spec, remote+"/"):
		return git.RemoteRefBranch, strings.TrimPrefix(spec, remote+"/"), nil
	}
	kind, err := git.ClassifyRemoteRef(remote, spec)
	if err != nil {
		return git.RemoteRefNone, "", fmt.Errorf("looking up base ref %q on %s: %w", spec, remote, err)
	}
	switch kind {
	case git.RemoteRefAmbiguous:
		return kind, "", fmt.Errorf("base ref %q is both a tag and a branch on %s; use refs/tags/%s or refs/heads/%s", spec, remote, spec, spec)
	case git.RemoteRefNone:
		return kind, "", fmt.Errorf("base ref %q is neither a tag nor a branch on %s", spec, remote)
	}
	return kind, spec, nil
}

// fetchBaseRef fetches the base ref from the remote and returns the ref to use
// locally and the commit it points at. The ref may move backwards or
// sideways, so it is never fast-forwarded into a local branch.
func fetchBaseRef(cfg *config.Config, remote, spec string) (ref, sha string, err error) {
	kind, name, err := classifyBaseRef(remote, spec)
	if err != nil {
		cfg.Errorf("%s", err)
		return "", "", ErrSilent
	}

	switch kind {
	case git.RemoteRefTag:
		ref = "refs/tags/" + name
		err = git.FetchTag(remote, name)
	default:
		ref = "refs/remotes/" + remote + "/" + name
		err = git.FetchBranch(remote, name)
	}
	if err != nil {
		if errors.Is(err, git.ErrRemoteRefNotFound) || errors.Is(err, git.ErrRemoteBranchNotFound) {
			cfg.Errorf("base ref %s no longer exists on %s", spec, remote)
		} else {
			cfg.Errorf("failed to fetch base ref %s from %s: %v", spec, remote, err)
		}
		return "", "", ErrSilent
	}

	// ^{commit} peels annotated tags to the commit they point at.
	sha, err = git.RevParse(ref + "^{commit}")
	if err != nil {
		cfg.Errorf("could not resolve base ref %s: %v", ref, err)
		return "", "", ErrSilent
	}
	cfg.Successf("Fetched %s from %s (%s)", spec, remote, short(sha))
	return ref, sha, nil
}

// resolveBaseRefTarget fetches the base ref and returns it as the target for
// the cascade. Local trunk is left untouched.
func resolveBaseRefTarget(cfg *config.Config, s *stack.Stack, remote, spec string, preflight func(string, bool) error) (trunkTarget, error) {
	ref, sha, err := fetchBaseRef(cfg, remote, spec)
	if err != nil {
		return trunkTarget{}, err
	}

	// The first active branch's recorded base is the target of the last
	// rebase, so a different SHA means the base ref has moved in either
	// direction.
	moved := false
	if i := s.FirstActiveBranchIndex(); i >= 0 {
		moved = s.Branches[i].Base != sha
	}
	if preflight != nil {
		if err := preflight(sha, moved); err != nil {
			return trunkTarget{}, err
		}
	}
	cfg.Printf("  Rebasing the stack onto %s; local %s is unchanged.", ref, s.Trunk.Branch)
	return trunkTarget{Branch: s.Trunk.Branch, Ref: ref, SHA: sha, Moved: moved}, nil
}

// baseRefCutoff returns the old base to replay the bottom branch from when it
// is moved onto a base ref, or "" when a plain rebase is enough.
//
// A plain `git rebase <target>` replays <target>..<branch>. If the base ref has
// moved backwards (a rolled-back stable tag), the target is an ancestor of the
// branch, so git considers the branch up to date and keeps the commits between
// the two. Rebasing --onto from the previously recorded base replays only the
// branch's own commits.
func baseRefCutoff(cfg *config.Config, s *stack.Stack, target trunkTarget) (string, error) {
	if i := s.FirstActiveBranchIndex(); i != 0 || !target.Moved {
		// Merged branches below the first active branch select their own
		// boundary from the merged PR's head.
		return "", nil
	}
	b := s.Branches[0]
	if valid, err := rebaseBoundaryContains(b.Base, b.Branch); err != nil {
		cfg.Errorf("%s", err)
		return "", ErrSilent
	} else if valid {
		return b.Base, nil
	}

	// Without a usable recorded base only a forward move is safe: a target
	// that is already inside the branch's history would be a silent no-op.
	inside, err := git.IsAncestor(target.SHA, b.Branch)
	if err != nil {
		cfg.Errorf("checking %s against %s: %v", b.Branch, target.Ref, err)
		return "", ErrSilent
	}
	if inside {
		cfg.Errorf("could not determine the previous base of %s to move it onto %s; rebase it manually with `git rebase --onto`", b.Branch, target.Ref)
		return "", ErrSilent
	}
	return "", nil
}
