package cmd

import (
	"path/filepath"
	"testing"

	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/stack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baseRefRepo is a clone whose main has moved past a "stable" tag:
//
//	c1 (stable) <- c2 (good) <- c3 (bad)   main
//	        \
//	         feat <- feat2                  the stack
type baseRefRepo struct {
	dir, gitDir string
	c1, c2, c3  string
}

func setupBaseRefRepo(t *testing.T) baseRefRepo {
	t.Helper()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	cloneDir := filepath.Join(t.TempDir(), "clone")

	issue250Git(t, ".", "-c", "safe.bareRepository=all", "init", "--bare", "-b", "main", remoteDir)
	issue250Git(t, ".", "-c", "safe.bareRepository=all", "-C", remoteDir, "config", "receive.advertisePushOptions", "true")
	issue250Git(t, ".", "clone", remoteDir, cloneDir)
	issue250Git(t, cloneDir, "config", "user.name", "Test")
	issue250Git(t, cloneDir, "config", "user.email", "test@example.com")

	commit := func(file, subject string) string {
		issue250WriteFile(t, cloneDir, file, subject+"\n")
		issue250Git(t, cloneDir, "add", ".")
		issue250Git(t, cloneDir, "commit", "-m", subject)
		return issue250Git(t, cloneDir, "rev-parse", "HEAD")
	}

	r := baseRefRepo{dir: cloneDir, gitDir: filepath.Join(cloneDir, ".git")}
	r.c1 = commit("c1.txt", "c1")
	issue250Git(t, cloneDir, "push", "-u", "origin", "main")
	issue250Git(t, cloneDir, "tag", "stable")
	issue250Git(t, cloneDir, "push", "origin", "refs/tags/stable")

	issue250Git(t, cloneDir, "checkout", "-b", "feat")
	feat := commit("feat.txt", "feat commit")
	issue250Git(t, cloneDir, "push", "-u", "origin", "feat")
	issue250Git(t, cloneDir, "checkout", "-b", "feat2")
	feat2 := commit("feat2.txt", "feat2 commit")
	issue250Git(t, cloneDir, "push", "-u", "origin", "feat2")

	writeStackFile(t, r.gitDir, stack.Stack{
		Trunk: stack.BranchRef{Branch: "main", Head: r.c1},
		Branches: []stack.BranchRef{
			{Branch: "feat", Head: feat, Base: r.c1},
			{Branch: "feat2", Head: feat2, Base: feat},
		},
	})

	// Main moves on: c2 passed CI, c3 did not.
	issue250Git(t, cloneDir, "checkout", "main")
	r.c2 = commit("c2.txt", "c2 good")
	r.c3 = commit("c3.txt", "c3 bad")
	issue250Git(t, cloneDir, "push", "origin", "main")
	issue250Git(t, cloneDir, "checkout", "feat2")
	return r
}

// moveStableTag moves the remote tag without touching the local one, as CI
// would.
func (r baseRefRepo) moveStableTag(t *testing.T, sha string) {
	t.Helper()
	issue250Git(t, r.dir, "push", "--force", "origin", sha+":refs/tags/stable")
}

func (r baseRefRepo) isAncestor(t *testing.T, ancestor, descendant string) bool {
	t.Helper()
	return issue250GitMayFail(t, r.dir, "merge-base", "--is-ancestor", ancestor, descendant) == nil
}

func (r baseRefRepo) recordedBase(t *testing.T, branch string) string {
	t.Helper()
	sf, err := stack.Load(r.gitDir)
	require.NoError(t, err)
	require.Len(t, sf.Stacks, 1)
	i := sf.Stacks[0].IndexOf(branch)
	require.GreaterOrEqual(t, i, 0)
	return sf.Stacks[0].Branches[i].Base
}

func (r baseRefRepo) assertStackedOn(t *testing.T, base string) {
	t.Helper()
	assert.Equal(t, base, issue250Git(t, r.dir, "rev-parse", "feat~1"), "feat must sit directly on %s", base)
	assert.Equal(t, "feat2 commit\nfeat commit", issue250Git(t, r.dir, "log", "--format=%s", base+"..feat2"))
}

func TestRebase_BaseRef_TagMovedForward(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	withIssue250Repo(t, r.dir)
	cfg := issue250TestConfig(t)

	require.NoError(t, runRebase(cfg, &rebaseOptions{remote: "origin", baseRef: "stable"}))

	r.assertStackedOn(t, r.c2)
	assert.False(t, r.isAncestor(t, r.c3, "feat"), "the bad commit on main must not enter the stack")
	assert.Equal(t, r.c2, r.recordedBase(t, "feat"))
	assert.Equal(t, r.c3, issue250Git(t, r.dir, "rev-parse", "main"), "local trunk must be left alone")
	assert.Equal(t, r.c2, issue250Git(t, r.dir, "rev-parse", "refs/tags/stable"), "the stale local tag is replaced")
}

func TestRebase_BaseRef_FullRefForms(t *testing.T) {
	for _, spec := range []string{"refs/tags/stable", "stable"} {
		t.Run(spec, func(t *testing.T) {
			r := setupBaseRefRepo(t)
			r.moveStableTag(t, r.c2)
			withIssue250Repo(t, r.dir)

			require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: spec}))
			r.assertStackedOn(t, r.c2)
		})
	}
}

func TestRebase_BaseRef_BranchRef(t *testing.T) {
	for _, spec := range []string{"refs/heads/release", "origin/release", "release"} {
		t.Run(spec, func(t *testing.T) {
			r := setupBaseRefRepo(t)
			issue250Git(t, r.dir, "push", "origin", r.c2+":refs/heads/release")
			withIssue250Repo(t, r.dir)

			require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: spec}))
			r.assertStackedOn(t, r.c2)
			assert.Equal(t, r.c2, r.recordedBase(t, "feat"))
		})
	}
}

func TestRebase_BaseRef_AnnotatedTagIsPeeledToCommit(t *testing.T) {
	r := setupBaseRefRepo(t)
	issue250Git(t, r.dir, "tag", "-a", "-m", "stable", "annotated", r.c2)
	issue250Git(t, r.dir, "push", "origin", "refs/tags/annotated")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "annotated"}))
	r.assertStackedOn(t, r.c2)
	assert.Equal(t, r.c2, r.recordedBase(t, "feat"), "the commit, not the tag object, is recorded")
}

// A stable tag that is rolled back to an ancestor of the stack must remove the
// commits between the two; a plain `git rebase` would call the branch up to date.
func TestRebase_BaseRef_TagRolledBack(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	withIssue250Repo(t, r.dir)
	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "stable"}))
	r.assertStackedOn(t, r.c2)

	r.moveStableTag(t, r.c1)
	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "stable"}))

	r.assertStackedOn(t, r.c1)
	assert.False(t, r.isAncestor(t, r.c2, "feat"), "commits above the rolled-back tag must be dropped")
	assert.Equal(t, r.c1, r.recordedBase(t, "feat"))
}

func TestRebase_BaseRef_RollbackWithoutRecordedBaseFailsSafely(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	withIssue250Repo(t, r.dir)
	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "stable"}))

	sf, err := stack.Load(r.gitDir)
	require.NoError(t, err)
	s := sf.Stacks[0]
	s.Branches[0].Base = ""
	writeStackFile(t, r.gitDir, s)
	featBefore := issue250Git(t, r.dir, "rev-parse", "feat")

	r.moveStableTag(t, r.c1)
	err = runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "stable"})

	require.ErrorIs(t, err, ErrSilent)
	assert.Equal(t, featBefore, issue250Git(t, r.dir, "rev-parse", "feat"))
}

func TestRebase_BaseRef_FromGitConfig(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	issue250Git(t, r.dir, "config", "gh-stack.base-ref", "refs/tags/stable")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin"}))
	r.assertStackedOn(t, r.c2)
}

func TestRebase_BaseRef_FlagOverridesGitConfig(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	issue250Git(t, r.dir, "push", "origin", r.c1+":refs/tags/other")
	issue250Git(t, r.dir, "config", "gh-stack.base-ref", "refs/tags/stable")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "other"}))
	r.assertStackedOn(t, r.c1)
}

func TestRebase_BaseRef_UnsetRebasesOntoTrunkHead(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	withIssue250Repo(t, r.dir)

	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin"}))
	r.assertStackedOn(t, r.c3)
}

func TestRebase_BaseRef_Errors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, r baseRefRepo)
		opts  rebaseOptions
		want  error
	}{
		{
			name: "missing on the remote",
			opts: rebaseOptions{baseRef: "nope"},
			want: ErrSilent,
		},
		{
			name: "both a tag and a branch",
			setup: func(t *testing.T, r baseRefRepo) {
				issue250Git(t, r.dir, "push", "origin", r.c2+":refs/heads/stable")
			},
			opts: rebaseOptions{baseRef: "stable"},
			want: ErrSilent,
		},
		{
			name: "unsupported full ref",
			opts: rebaseOptions{baseRef: "refs/notes/stable"},
			want: ErrSilent,
		},
		{
			name: "with --no-trunk",
			opts: rebaseOptions{baseRef: "stable", noTrunk: true},
			want: ErrInvalidArgs,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupBaseRefRepo(t)
			if tt.setup != nil {
				tt.setup(t, r)
			}
			withIssue250Repo(t, r.dir)
			featBefore := issue250Git(t, r.dir, "rev-parse", "feat")
			opts := tt.opts
			opts.remote = "origin"

			require.ErrorIs(t, runRebase(issue250TestConfig(t), &opts), tt.want)
			assert.Equal(t, featBefore, issue250Git(t, r.dir, "rev-parse", "feat"), "nothing is rebased on failure")
		})
	}
}

func TestClassifyBaseRef(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		classify git.RemoteRefKind
		wantKind git.RemoteRefKind
		wantName string
		wantErr  bool
	}{
		{name: "full tag", spec: "refs/tags/stable", wantKind: git.RemoteRefTag, wantName: "stable"},
		{name: "full tag with slash", spec: "refs/tags/ci/stable", wantKind: git.RemoteRefTag, wantName: "ci/stable"},
		{name: "full branch", spec: "refs/heads/stable", wantKind: git.RemoteRefBranch, wantName: "stable"},
		{name: "remote-qualified branch", spec: "origin/stable", wantKind: git.RemoteRefBranch, wantName: "stable"},
		{name: "bare tag", spec: "stable", classify: git.RemoteRefTag, wantKind: git.RemoteRefTag, wantName: "stable"},
		{name: "bare branch", spec: "stable", classify: git.RemoteRefBranch, wantKind: git.RemoteRefBranch, wantName: "stable"},
		{name: "ambiguous", spec: "stable", classify: git.RemoteRefAmbiguous, wantErr: true},
		{name: "missing", spec: "stable", classify: git.RemoteRefNone, wantErr: true},
		{name: "other namespace", spec: "refs/notes/x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := git.SetOps(&git.MockOps{
				ClassifyRemoteRefFn: func(remote, name string) (git.RemoteRefKind, error) {
					assert.Equal(t, "origin", remote)
					return tt.classify, nil
				},
			})
			defer restore()

			kind, name, err := classifyBaseRef("origin", tt.spec)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

func TestEffectiveBaseRef(t *testing.T) {
	restore := git.SetOps(&git.MockOps{
		ConfiguredBaseRefFn: func() (string, error) { return "refs/tags/configured", nil },
	})
	defer restore()

	got, err := effectiveBaseRef("refs/tags/flag")
	require.NoError(t, err)
	assert.Equal(t, "refs/tags/flag", got, "the flag wins")

	got, err = effectiveBaseRef("")
	require.NoError(t, err)
	assert.Equal(t, "refs/tags/configured", got)
}

func TestSync_BaseRef_RebasesOntoTagAndPushes(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	withIssue250Repo(t, r.dir)

	require.NoError(t, runSync(issue250TestConfig(t), &syncOptions{remote: "origin", baseRef: "stable"}))

	r.assertStackedOn(t, r.c2)
	assert.Equal(t, r.c3, issue250Git(t, r.dir, "rev-parse", "main"), "local trunk must be left alone")
	assert.Equal(t, r.c2, r.recordedBase(t, "feat"))
	assert.Equal(t, issue250Git(t, r.dir, "rev-parse", "feat2"), issue250Git(t, r.dir, "rev-parse", "origin/feat2"), "rebased branches are pushed")
}

func TestSync_BaseRef_RolledBackTag(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	withIssue250Repo(t, r.dir)
	opts := &syncOptions{remote: "origin", baseRef: "stable"}
	require.NoError(t, runSync(issue250TestConfig(t), opts))

	r.moveStableTag(t, r.c1)
	require.NoError(t, runSync(issue250TestConfig(t), opts))

	r.assertStackedOn(t, r.c1)
	assert.False(t, r.isAncestor(t, r.c2, "feat"))
}

func TestSync_BaseRef_UnchangedTagDoesNotRewriteHistory(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	withIssue250Repo(t, r.dir)
	opts := &syncOptions{remote: "origin", baseRef: "stable"}
	require.NoError(t, runSync(issue250TestConfig(t), opts))
	before := issue250Git(t, r.dir, "rev-parse", "feat", "feat2")

	require.NoError(t, runSync(issue250TestConfig(t), opts))

	assert.Equal(t, before, issue250Git(t, r.dir, "rev-parse", "feat", "feat2"))
}

func TestSync_BaseRef_FromGitConfig(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	issue250Git(t, r.dir, "config", "gh-stack.base-ref", "refs/tags/stable")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runSync(issue250TestConfig(t), &syncOptions{remote: "origin"}))
	r.assertStackedOn(t, r.c2)
}

// A conflict pauses the rebase with the target pinned in the journal. Moving
// the tag again before --continue must not change where the stack lands.
func TestRebase_BaseRef_ContinueKeepsPinnedTarget(t *testing.T) {
	r := setupBaseRefRepo(t)

	// A stable commit that conflicts with feat's file.
	issue250Git(t, r.dir, "checkout", "-b", "side", r.c1)
	issue250WriteFile(t, r.dir, "feat.txt", "conflicting\n")
	issue250Git(t, r.dir, "add", ".")
	issue250Git(t, r.dir, "commit", "-m", "stable conflicts with feat")
	pinned := issue250Git(t, r.dir, "rev-parse", "HEAD")
	issue250Git(t, r.dir, "checkout", "feat2")
	r.moveStableTag(t, pinned)
	withIssue250Repo(t, r.dir)

	require.ErrorIs(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "stable"}), ErrConflict)
	state, err := loadRebaseState(r.gitDir)
	require.NoError(t, err)
	assert.Equal(t, pinned, state.TrunkSHA)

	r.moveStableTag(t, r.c2)
	issue250WriteFile(t, r.dir, "feat.txt", "resolved\n")
	issue250Git(t, r.dir, "add", "feat.txt")
	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{cont: true}))

	r.assertStackedOn(t, pinned)
	assert.False(t, r.isAncestor(t, r.c2, "feat"), "the tag's later position must not leak in")
	assert.Equal(t, pinned, r.recordedBase(t, "feat"))
	assert.NoFileExists(t, filepath.Join(r.gitDir, rebaseStateFile))
}

// The bottom PR merged to main, but its merge commit has not reached stable.
// The rest of the stack is rebased onto stable regardless, dropping the merged
// branch's commits.
func TestRebase_BaseRef_MergedBottomNotYetInStable(t *testing.T) {
	r := setupBaseRefRepo(t)
	sf, err := stack.Load(r.gitDir)
	require.NoError(t, err)
	s := sf.Stacks[0]
	s.Branches[0].PullRequest = &stack.PullRequestRef{Number: 1, Merged: true, MergedHead: s.Branches[0].Head}
	writeStackFile(t, r.gitDir, s)
	withIssue250Repo(t, r.dir)

	require.NoError(t, runRebase(issue250TestConfig(t), &rebaseOptions{remote: "origin", baseRef: "stable"}))

	assert.Equal(t, r.c1, issue250Git(t, r.dir, "rev-parse", "feat2~1"))
	assert.Equal(t, "feat2 commit", issue250Git(t, r.dir, "log", "--format=%s", r.c1+"..feat2"))
	assert.False(t, r.isAncestor(t, r.c3, "feat2"))
}

func TestInit_BaseRef_CreatesBranchesFromRef(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	issue250Git(t, r.dir, "checkout", "main")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runInit(issue250TestConfig(t), &initOptions{branches: []string{"a", "b"}, baseRef: "stable"}))

	assert.Equal(t, r.c2, issue250Git(t, r.dir, "rev-parse", "a"), "the first branch starts at the tag, not main")
	assert.Equal(t, r.c2, issue250Git(t, r.dir, "rev-parse", "b"))
	assert.Equal(t, "b", issue250Git(t, r.dir, "branch", "--show-current"))

	sf, err := stack.Load(r.gitDir)
	require.NoError(t, err)
	s := stackWithBranch(t, sf, "a")
	require.NotNil(t, s)
	assert.Equal(t, "main", s.Trunk.Branch, "pull requests still target the trunk")
	assert.Equal(t, r.c2, s.Trunk.Head)
	assert.Equal(t, r.c2, s.Branches[0].Base)
}

func TestInit_BaseRef_AdoptedBranchRecordsForkFromRef(t *testing.T) {
	r := setupBaseRefRepo(t)
	issue250Git(t, r.dir, "checkout", "-b", "existing", r.c1)
	issue250WriteFile(t, r.dir, "existing.txt", "x\n")
	issue250Git(t, r.dir, "add", ".")
	issue250Git(t, r.dir, "commit", "-m", "existing commit")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runInit(issue250TestConfig(t), &initOptions{branches: []string{"existing"}, baseRef: "stable"}))

	sf, err := stack.Load(r.gitDir)
	require.NoError(t, err)
	s := stackWithBranch(t, sf, "existing")
	require.NotNil(t, s)
	assert.Equal(t, r.c1, s.Branches[0].Base)
}

func TestInit_BaseRef_FromGitConfig(t *testing.T) {
	r := setupBaseRefRepo(t)
	r.moveStableTag(t, r.c2)
	issue250Git(t, r.dir, "config", "gh-stack.base-ref", "refs/tags/stable")
	issue250Git(t, r.dir, "checkout", "main")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runInit(issue250TestConfig(t), &initOptions{branches: []string{"a"}}))
	assert.Equal(t, r.c2, issue250Git(t, r.dir, "rev-parse", "a"))
}

func TestInit_BaseRef_UnsetBranchesFromTrunk(t *testing.T) {
	r := setupBaseRefRepo(t)
	issue250Git(t, r.dir, "checkout", "main")
	withIssue250Repo(t, r.dir)

	require.NoError(t, runInit(issue250TestConfig(t), &initOptions{branches: []string{"a"}}))
	assert.Equal(t, r.c3, issue250Git(t, r.dir, "rev-parse", "a"))
}

func TestInit_BaseRef_MissingRefCreatesNothing(t *testing.T) {
	r := setupBaseRefRepo(t)
	issue250Git(t, r.dir, "checkout", "main")
	withIssue250Repo(t, r.dir)

	require.ErrorIs(t, runInit(issue250TestConfig(t), &initOptions{branches: []string{"a"}, baseRef: "nope"}), ErrSilent)
	assert.Error(t, issue250GitMayFail(t, r.dir, "rev-parse", "--verify", "refs/heads/a"))
}

func stackWithBranch(t *testing.T, sf *stack.StackFile, branch string) *stack.Stack {
	t.Helper()
	stacks := sf.FindAllStacksForBranch(branch)
	require.Len(t, stacks, 1)
	return stacks[0]
}
