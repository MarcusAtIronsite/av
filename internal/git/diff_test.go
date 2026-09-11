package git_test

import (
	"testing"

	"github.com/aviator-co/av/internal/git"
	"github.com/aviator-co/av/internal/git/gittest"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestRepoDiffNumstat(t *testing.T) {
	repo := gittest.NewTempRepo(t)

	// Committed to main so that shrinking it on "foo" produces deletions.
	repo.CommitFile(t, "shrink.txt", "line1\nline2\nline3\n")

	repo.CreateRef(t, plumbing.NewBranchReferenceName("foo"))
	repo.CheckoutBranch(t, plumbing.NewBranchReferenceName("foo"))

	repo.CommitFile(t, "shrink.txt", "line1\n")
	repo.CommitFile(t, "added.txt", "line1\nline2\nline3\n")

	stats, err := repo.AsAvGitRepo().DiffNumstat(t.Context(), "main", "foo")
	require.NoError(t, err)

	byPath := make(map[string]git.FileDiffStat)
	for _, s := range stats {
		byPath[s.Path] = s
	}
	require.Equal(t, git.FileDiffStat{Path: "added.txt", Additions: 3, Deletions: 0}, byPath["added.txt"])
	require.Equal(t, git.FileDiffStat{Path: "shrink.txt", Additions: 0, Deletions: 2}, byPath["shrink.txt"])
}

func TestRepoDiffAmbiguousPathName(t *testing.T) {
	repo := gittest.NewTempRepo(t)

	repo.CreateRef(t, plumbing.NewBranchReferenceName("foo"))
	repo.CheckoutBranch(t, plumbing.NewBranchReferenceName("foo"))

	repo.CommitFile(t, "foo", "foo")
	diff, err := repo.AsAvGitRepo().Diff(t.Context(), &git.DiffOpts{
		Quiet:      true,
		Specifiers: []string{"main", "foo"},
	})
	require.NoError(t, err, "repo.Diff should not error given an ambiguous branch/path name")
	require.False(
		t,
		diff.Empty,
		"diff between branches with different trees should return non-empty",
	)
}
