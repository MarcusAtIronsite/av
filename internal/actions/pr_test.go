package actions_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/aviator-co/av/internal/actions"
	"github.com/aviator-co/av/internal/config"
	"github.com/aviator-co/av/internal/git/gittest"
	"github.com/aviator-co/av/internal/meta"
	"github.com/aviator-co/av/internal/utils/maputils"
	"github.com/aviator-co/av/internal/utils/stackutils"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadPRMetadata(t *testing.T) {
	tx := fakeReadTx{}
	prMeta := actions.PRMetadata{
		Parent:     "foo",
		ParentHead: "bar",
		ParentPull: 123,
		Trunk:      "baz",
	}
	prBody := actions.AddPRMetadataAndStack(
		"Hello! This is a cool PR that does some neat things.",
		prMeta,
		"foo",
		nil,
		tx,
		nil,
		nil,
	)
	prMeta2, err := actions.ReadPRMetadata(prBody)
	require.NoError(t, err)
	assert.Equal(t, prMeta.Parent, prMeta2.Parent)
	assert.Equal(t, prMeta.ParentHead, prMeta2.ParentHead)
	assert.Equal(t, prMeta.ParentPull, prMeta2.ParentPull)
	assert.Equal(t, prMeta.Trunk, prMeta2.Trunk)

	prBody = actions.AddPRMetadataAndStack(prBody, actions.PRMetadata{
		Parent:     "foo2",
		ParentHead: "bar2",
		ParentPull: 1234,
		Trunk:      "baz2",
	}, "foo2", nil, tx, nil, nil)
	assert.Contains(t, prBody, "Hello! This is a cool PR that does some neat things.\n\n")
	prMeta2, err = actions.ReadPRMetadata(prBody)
	require.NoError(t, err)
	assert.Equal(t, "foo2", prMeta2.Parent)
	assert.Equal(t, "bar2", prMeta2.ParentHead)
}

func TestPRMetadataPreservesBody(t *testing.T) {
	tx := fakeReadTx{}
	sampleMeta := actions.PRMetadata{
		Parent:     "foo",
		ParentHead: "bar",
		ParentPull: 123,
		Trunk:      "baz",
	}
	body1 := actions.AddPRMetadataAndStack(
		"Hello! This is a cool PR that does some neat things.",
		sampleMeta,
		"foo",
		nil,
		tx,
		nil,
		nil,
	)
	// Add some text to the end of the body (as if someone had edited manually)
	body1 += "\n\nIt's very neat, actually."

	body2 := actions.AddPRMetadataAndStack(body1, sampleMeta, "foo", nil, tx, nil, nil)
	assert.Contains(t, body2, "Hello! This is a cool PR that does some neat things.")
	assert.Contains(t, body2, "It's very neat, actually.")
	assert.Contains(t, body2, "\n"+actions.PRMetadataCommentStart)
}

func TestPRWithStack(t *testing.T) {
	tx := fakeReadTx{
		"baz": {
			Name: "baz",
			Parent: meta.BranchState{
				Name:  "main",
				Trunk: true,
			},
			PullRequest: &meta.PullRequest{
				Number:    1001,
				Permalink: "https://github.com/org/repo/pull/1001",
			},
		},
		"foo": {
			Name: "foo",
			Parent: meta.BranchState{
				Name: "baz",
			},
			PullRequest: &meta.PullRequest{
				Number:    1002,
				Permalink: "https://github.com/org/repo/pull/1002",
			},
		},
	}
	stack := &stackutils.StackTreeNode{
		Branch: &stackutils.StackTreeBranchInfo{
			BranchName: "main",
		},
		Children: []*stackutils.StackTreeNode{
			{
				Branch: &stackutils.StackTreeBranchInfo{
					BranchName: "baz",
				},
				Children: []*stackutils.StackTreeNode{
					{
						Branch: &stackutils.StackTreeBranchInfo{
							BranchName: "foo",
						},
						Children: []*stackutils.StackTreeNode{},
					},
				},
			},
		},
	}

	sampleMeta := actions.PRMetadata{
		Parent:     "foo",
		ParentHead: "bar",
		ParentPull: 123,
		Trunk:      "baz",
	}
	body1 := actions.AddPRMetadataAndStack(
		"Hello! This is a cool PR that does some neat things.",
		sampleMeta,
		"foo",
		stack,
		tx,
		nil,
		nil,
	)

	assert.Equal(t, `<!-- av pr stack begin -->
<table><tr><td>

<details><summary><b>Depends on #1001.</b> This PR is part of a stack created with <a href="https://github.com/aviator-co/av">Aviator</a>.</summary>

* ➡️ **#1002**
* **#1001**
* `+"`"+`main`+"`"+`
</details>

</td></tr></table>
<!-- av pr stack end -->

Hello! This is a cool PR that does some neat things.

<!-- av pr metadata
This information is embedded by the av CLI when creating PRs to track the status of stacks when using Aviator. Please do not delete or edit this section of the PR.
`+"```"+`
{"parent":"foo","parentHead":"bar","parentPull":123,"trunk":"baz"}
`+"```"+`
-->
`, body1)
}

func TestPRWithForkedStack(t *testing.T) {
	tx := fakeReadTx{
		"baz": {
			Name: "baz",
			Parent: meta.BranchState{
				Name:  "main",
				Trunk: true,
			},
			PullRequest: &meta.PullRequest{
				Number:    1001,
				Permalink: "https://github.com/org/repo/pull/1001",
			},
		},
		"foo": {
			Name: "foo",
			Parent: meta.BranchState{
				Name: "baz",
			},
			PullRequest: &meta.PullRequest{
				Number:    1002,
				Permalink: "https://github.com/org/repo/pull/1002",
			},
		},
		"qux": {
			Name: "qux",
			Parent: meta.BranchState{
				Name:  "main",
				Trunk: true,
			},
			PullRequest: &meta.PullRequest{
				Number:    1003,
				Permalink: "https://github.com/org/repo/pull/1003",
			},
		},
	}
	stack := &stackutils.StackTreeNode{
		Branch: &stackutils.StackTreeBranchInfo{
			BranchName: "main",
		},
		Children: []*stackutils.StackTreeNode{
			{
				Branch: &stackutils.StackTreeBranchInfo{
					BranchName: "baz",
				},
				Children: []*stackutils.StackTreeNode{
					{
						Branch: &stackutils.StackTreeBranchInfo{
							BranchName: "foo",
						},
						Children: []*stackutils.StackTreeNode{},
					},
				},
			},
			{
				Branch: &stackutils.StackTreeBranchInfo{
					BranchName: "qux",
				},
				Children: []*stackutils.StackTreeNode{},
			},
		},
	}

	sampleMeta := actions.PRMetadata{
		Parent:     "foo",
		ParentHead: "bar",
		ParentPull: 123,
		Trunk:      "baz",
	}
	body1 := actions.AddPRMetadataAndStack(
		"Hello! This is a cool PR that does some neat things.",
		sampleMeta,
		"foo",
		stack,
		tx,
		nil,
		nil,
	)

	assert.Equal(t, `<!-- av pr stack begin -->
<table><tr><td>

<details><summary><b>Depends on #1001.</b> This PR is part of a stack created with <a href="https://github.com/aviator-co/av">Aviator</a>.</summary>

* `+"`"+`main`+"`"+`
  * **#1001**
    * ➡️ **#1002**
  * **#1003**
</details>

</td></tr></table>
<!-- av pr stack end -->

Hello! This is a cool PR that does some neat things.

<!-- av pr metadata
This information is embedded by the av CLI when creating PRs to track the status of stacks when using Aviator. Please do not delete or edit this section of the PR.
`+"```"+`
{"parent":"foo","parentHead":"bar","parentPull":123,"trunk":"baz"}
`+"```"+`
-->
`, body1)
}

func TestPRWithStackDiffStat(t *testing.T) {
	tx := fakeReadTx{
		"baz": {
			Name: "baz",
			Parent: meta.BranchState{
				Name:  "main",
				Trunk: true,
			},
			PullRequest: &meta.PullRequest{
				Number:    1001,
				Permalink: "https://github.com/org/repo/pull/1001",
			},
		},
		"foo": {
			Name: "foo",
			Parent: meta.BranchState{
				Name: "baz",
			},
			PullRequest: &meta.PullRequest{
				Number:    1002,
				Permalink: "https://github.com/org/repo/pull/1002",
			},
		},
	}
	stack := &stackutils.StackTreeNode{
		Branch: &stackutils.StackTreeBranchInfo{
			BranchName: "main",
		},
		Children: []*stackutils.StackTreeNode{
			{
				Branch: &stackutils.StackTreeBranchInfo{
					BranchName: "baz",
				},
				Children: []*stackutils.StackTreeNode{
					{
						Branch: &stackutils.StackTreeBranchInfo{
							BranchName: "foo",
						},
						Children: []*stackutils.StackTreeNode{},
					},
				},
			},
		},
	}

	sampleMeta := actions.PRMetadata{
		Parent:     "foo",
		ParentHead: "bar",
		ParentPull: 123,
		Trunk:      "baz",
	}
	body1 := actions.AddPRMetadataAndStack(
		"Hello! This is a cool PR that does some neat things.",
		sampleMeta,
		"foo",
		stack,
		tx,
		map[string]actions.LineStat{
			"baz": {Additions: 170, Deletions: 7},
			"foo": {Additions: 12, Deletions: 3},
		},
		[]actions.CategoryLineStat{
			{Name: "Testing Files", LineStat: actions.LineStat{Additions: 100, Deletions: 5}},
			{Name: "Generated Files", LineStat: actions.LineStat{Additions: 9000, Deletions: 1000}},
		},
	)

	expected := "<!-- av pr stack begin -->\n" +
		"<table><tr><td>\n\n" +
		"<details><summary><b>Depends on #1001.</b> This PR is part of a stack created with <a href=\"https://github.com/aviator-co/av\">Aviator</a>.</summary>\n\n" +
		"* ➡️ **#1002** (+12 -3)\n" +
		"* **#1001** (+170 -7)\n" +
		"* `main`\n" +
		"</details>\n\n" +
		"### PR Breakdown\n\n" +
		"| Breakdown | Changes | |\n" +
		"|:--|--:|---|\n" +
		"| **Generated Files** | `+9,000 -1,000` | `██████████ 99%` |\n" +
		"| **Testing Files** | `+100 -5` | `█░░░░░░░░░ 1%` |\n" +
		"\n" +
		"</td></tr></table>\n" +
		"<!-- av pr stack end -->\n\n" +
		"Hello! This is a cool PR that does some neat things.\n\n" +
		"<!-- av pr metadata\n" +
		"This information is embedded by the av CLI when creating PRs to track the status of stacks when using Aviator. Please do not delete or edit this section of the PR.\n" +
		"```\n" +
		"{\"parent\":\"foo\",\"parentHead\":\"bar\",\"parentPull\":123,\"trunk\":\"baz\"}\n" +
		"```\n" +
		"-->\n"
	assert.Equal(t, expected, body1)
}

func TestComputeCategoryDiffStats(t *testing.T) {
	repo := gittest.NewTempRepo(t)

	repo.CreateRef(t, plumbing.NewBranchReferenceName("foo"))
	repo.CheckoutBranch(t, plumbing.NewBranchReferenceName("foo"))

	repo.CommitFile(t, "foo_test.go", "a\nb\nc\nd\ne\n")
	repo.CommitFile(t, "gen.pb.go", "1\n2\n3\n")
	repo.CommitFile(t, "gen_test.go", "x\ny\n")
	repo.CommitFile(t, "notes.md", "r1\nr2\nr3\nr4\n")

	categories := []config.DiffStatCategory{
		{Name: "Tests", Globs: []string{"**_test.go"}},
		{Name: "Generated", Globs: []string{"**.pb.go"}},
	}

	stats, err := actions.ComputeCategoryDiffStats(
		t.Context(), repo.AsAvGitRepo(), "main", "foo", categories, "Other",
	)
	require.NoError(t, err)
	require.Equal(t, []actions.CategoryLineStat{
		{
			Name:     "Tests",
			LineStat: actions.LineStat{Additions: 7},
			Files: []actions.FileLineStat{
				{Path: "foo_test.go", LineStat: actions.LineStat{Additions: 5}},
				{Path: "gen_test.go", LineStat: actions.LineStat{Additions: 2}},
			},
		},
		{
			Name:     "Generated",
			LineStat: actions.LineStat{Additions: 3},
			Files: []actions.FileLineStat{
				{Path: "gen.pb.go", LineStat: actions.LineStat{Additions: 3}},
			},
		},
		{
			Name:     "Other",
			LineStat: actions.LineStat{Additions: 4},
			Files: []actions.FileLineStat{
				{Path: "notes.md", LineStat: actions.LineStat{Additions: 4}},
			},
		},
	}, stats)
}

func TestComputeCategoryDiffStatsOmitsZeroAndDisabledFallback(t *testing.T) {
	repo := gittest.NewTempRepo(t)

	repo.CreateRef(t, plumbing.NewBranchReferenceName("foo"))
	repo.CheckoutBranch(t, plumbing.NewBranchReferenceName("foo"))

	repo.CommitFile(t, "foo_test.go", "a\nb\nc\n")

	categories := []config.DiffStatCategory{
		{Name: "Tests", Globs: []string{"**_test.go"}},
		{Name: "Generated", Globs: []string{"**.pb.go"}},
	}

	stats, err := actions.ComputeCategoryDiffStats(
		t.Context(), repo.AsAvGitRepo(), "main", "foo", categories, "",
	)
	require.NoError(t, err)
	require.Equal(t, []actions.CategoryLineStat{
		{
			Name:     "Tests",
			LineStat: actions.LineStat{Additions: 3},
			Files: []actions.FileLineStat{
				{Path: "foo_test.go", LineStat: actions.LineStat{Additions: 3}},
			},
		},
	}, stats)
}

func TestComputeCategoryDiffStatsFirstMatchWins(t *testing.T) {
	repo := gittest.NewTempRepo(t)

	repo.CreateRef(t, plumbing.NewBranchReferenceName("foo"))
	repo.CheckoutBranch(t, plumbing.NewBranchReferenceName("foo"))

	repo.CommitFile(t, "foo_test.go", "a\nb\nc\nd\ne\n")
	repo.CommitFile(t, "main.go", "1\n2\n3\n")

	// "Tests" is ordered before "All Go"; foo_test.go matches both patterns
	// but must count only towards Tests.
	categories := []config.DiffStatCategory{
		{Name: "Tests", Globs: []string{"**_test.go"}},
		{Name: "All Go", Globs: []string{"**.go"}},
	}

	stats, err := actions.ComputeCategoryDiffStats(
		t.Context(), repo.AsAvGitRepo(), "main", "foo", categories, "",
	)
	require.NoError(t, err)
	require.Equal(t, []actions.CategoryLineStat{
		{
			Name:     "Tests",
			LineStat: actions.LineStat{Additions: 5},
			Files: []actions.FileLineStat{
				{Path: "foo_test.go", LineStat: actions.LineStat{Additions: 5}},
			},
		},
		{
			Name:     "All Go",
			LineStat: actions.LineStat{Additions: 3},
			Files: []actions.FileLineStat{
				{Path: "main.go", LineStat: actions.LineStat{Additions: 3}},
			},
		},
	}, stats)
}

func TestComputeCategoryDiffStatsUsesMergeBase(t *testing.T) {
	repo := gittest.NewTempRepo(t)

	// Branch "foo" off main, then commit a file onto it.
	repo.CreateRef(t, plumbing.NewBranchReferenceName("foo"))
	repo.CheckoutBranch(t, plumbing.NewBranchReferenceName("foo"))
	repo.CommitFile(t, "foo_only.go", "x\ny\nz\n")

	// Advance main past the branch point. Committing to main after "foo"
	// branched must not leak into foo's diff, which GitHub computes from the
	// merge-base rather than main's tip.
	repo.CheckoutBranch(t, plumbing.NewBranchReferenceName("main"))
	repo.CommitFile(t, "main_only.go", "m1\nm2\nm3\nm4\nm5\n")

	categories := []config.DiffStatCategory{{Name: "All", Globs: []string{"**"}}}

	stats, err := actions.ComputeCategoryDiffStats(
		t.Context(), repo.AsAvGitRepo(), "main", "foo", categories, "",
	)
	require.NoError(t, err)
	require.Equal(t, []actions.CategoryLineStat{
		{
			Name:     "All",
			LineStat: actions.LineStat{Additions: 3},
			Files: []actions.FileLineStat{
				{Path: "foo_only.go", LineStat: actions.LineStat{Additions: 3}},
			},
		},
	}, stats)
}

func TestPRSingleWithCategoryBreakdown(t *testing.T) {
	tx := fakeReadTx{}

	// A single-PR "stack" (trunk with one child and no grandchildren) must not
	// render the stack list, but must still render the category breakdown.
	stack := &stackutils.StackTreeNode{
		Branch: &stackutils.StackTreeBranchInfo{BranchName: "main"},
		Children: []*stackutils.StackTreeNode{
			{
				Branch:   &stackutils.StackTreeBranchInfo{BranchName: "foo"},
				Children: []*stackutils.StackTreeNode{},
			},
		},
	}

	sampleMeta := actions.PRMetadata{
		Parent:     "main",
		ParentHead: "bar",
		Trunk:      "main",
	}

	body := actions.AddPRMetadataAndStack(
		"Hello! This is a cool PR that does some neat things.",
		sampleMeta,
		"foo",
		stack,
		tx,
		nil,
		[]actions.CategoryLineStat{
			{Name: "Tests", LineStat: actions.LineStat{Additions: 12, Deletions: 3}},
			{Name: "Other", LineStat: actions.LineStat{Additions: 40, Deletions: 1}},
		},
	)

	expected := "<!-- av pr stack begin -->\n" +
		"<table><tr><td>\n\n" +
		"### PR Breakdown\n\n" +
		"| Breakdown | Changes | |\n" +
		"|:--|--:|---|\n" +
		"| **Other** | `+40 -1` | `██████████ 73%` |\n" +
		"| **Tests** | `+12 -3` | `████░░░░░░ 27%` |\n" +
		"\n" +
		"</td></tr></table>\n" +
		"<!-- av pr stack end -->\n\n" +
		"Hello! This is a cool PR that does some neat things.\n\n" +
		"<!-- av pr metadata\n" +
		"This information is embedded by the av CLI when creating PRs to track the status of stacks when using Aviator. Please do not delete or edit this section of the PR.\n" +
		"```\n" +
		"{\"parent\":\"main\",\"parentHead\":\"bar\",\"trunk\":\"main\"}\n" +
		"```\n" +
		"-->\n"
	assert.Equal(t, expected, body)
}

type fakeReadTx map[string]meta.Branch

func (tx fakeReadTx) Repository() meta.Repository {
	return meta.Repository{}
}

func (tx fakeReadTx) Branch(name string) (meta.Branch, bool) {
	branch, ok := tx[name]
	if branch.Name == "" {
		branch.Name = name
	}
	return branch, ok
}

func (tx fakeReadTx) AllBranches() map[string]meta.Branch {
	return maputils.Copy(tx)
}

func TestPRBreakdownRevealsFilesWithLinks(t *testing.T) {
	tx := fakeReadTx{
		"foo": {
			Name: "foo",
			PullRequest: &meta.PullRequest{
				Number:    1002,
				Permalink: "https://github.com/org/repo/pull/1002",
			},
		},
	}

	sampleMeta := actions.PRMetadata{
		Parent: "main",
		Trunk:  "main",
	}

	body := actions.AddPRMetadataAndStack(
		"Hello!",
		sampleMeta,
		"foo",
		nil,
		tx,
		nil,
		[]actions.CategoryLineStat{
			{
				Name:     "TypeScript",
				LineStat: actions.LineStat{Additions: 100, Deletions: 20},
				Files: []actions.FileLineStat{
					{Path: "src/big.ts", LineStat: actions.LineStat{Additions: 80, Deletions: 10}},
					{Path: "src/small.ts", LineStat: actions.LineStat{Additions: 20, Deletions: 10}},
				},
			},
			{
				Name:     "Single",
				LineStat: actions.LineStat{Additions: 5, Deletions: 1},
				Files: []actions.FileLineStat{
					{Path: "only.go", LineStat: actions.LineStat{Additions: 5, Deletions: 1}},
				},
			},
		},
	)

	// The rollup table is always visible. A collapsed "Detailed Breakdown"
	// relists each category (no changes/bar column) with files indented beneath.
	assert.Contains(t, body, "| Breakdown | Changes | |")
	assert.Contains(t, body, "<table><tr><td>")
	assert.Contains(t, body, "<details><summary>Detailed Breakdown</summary>")
	assert.Contains(t, body, "| **TypeScript** | 2 files | |")
	assert.Contains(t, body, "| **Single** | 1 file | |")

	sum := sha256.Sum256([]byte("src/big.ts"))
	want := "https://github.com/org/repo/pull/1002/files#diff-" + hex.EncodeToString(sum[:])
	assert.Contains(t, body, "[src/big.ts]("+want+")")
	assert.Contains(t, body, "`██████████ 75%` |")
}

func TestPRBreakdownTruncatesLongFilenames(t *testing.T) {
	tx := fakeReadTx{"foo": {Name: "foo", PullRequest: &meta.PullRequest{
		Number: 1002, Permalink: "https://github.com/org/repo/pull/1002",
	}}}
	long := "a/very/long/directory/structure/that/goes/on/and/on/file.ts"

	body := actions.AddPRMetadataAndStack(
		"Hello!",
		actions.PRMetadata{Parent: "main", Trunk: "main"},
		"foo",
		nil,
		tx,
		nil,
		[]actions.CategoryLineStat{
			{
				Name:     "TypeScript",
				LineStat: actions.LineStat{Additions: 10},
				Files:    []actions.FileLineStat{{Path: long, LineStat: actions.LineStat{Additions: 10}}},
			},
		},
	)

	// The displayed path is truncated with a middle ellipsis; the diff anchor
	// still hashes the full repository-relative path.
	sum := sha256.Sum256([]byte(long))
	frag := hex.EncodeToString(sum[:])
	assert.Contains(t, body, "(https://github.com/org/repo/pull/1002/files#diff-"+frag+")")
	assert.Contains(t, body, "…")
	assert.NotContains(t, body, "["+long+"]")
}
