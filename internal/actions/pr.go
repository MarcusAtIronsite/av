package actions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/aviator-co/av/internal/utils/errutils"

	"emperror.dev/errors"
	"github.com/aviator-co/av/internal/config"
	"github.com/aviator-co/av/internal/editor"
	"github.com/aviator-co/av/internal/gh"
	"github.com/aviator-co/av/internal/git"
	"github.com/aviator-co/av/internal/meta"
	"github.com/aviator-co/av/internal/utils/browser"
	"github.com/aviator-co/av/internal/utils/colors"
	"github.com/aviator-co/av/internal/utils/sanitize"
	"github.com/aviator-co/av/internal/utils/stackutils"
	"github.com/aviator-co/av/internal/utils/stringutils"
	"github.com/aviator-co/av/internal/utils/templateutils"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/gobwas/glob"
	"github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
)

var ErrNoPRMetadata = errors.Sentinel("no PR metadata found")

type CreatePullRequestOpts struct {
	// The HEAD branch to create a pull request for.
	BranchName string
	// The pull request title.
	Title string
	// The pull request body (description).
	Body string
	// If true, create the pull request as a GitHub draft PR.
	Draft bool
	// If true, do not push the branch to GitHub
	NoPush bool
	// If true, force push the branch to GitHub
	ForcePush bool
	// If true, create a PR even if we think one already exists
	Force bool
	// If true, open an editor for editing the title and body
	Edit bool
	// If true, do not open the browser after creating the PR
	NoOpenBrowser bool
}

type CreatePullRequestResult struct {
	// True if the pull request was created
	Created bool
	// The (updated) branch metadata.
	Branch meta.Branch
	// The pull request object that was returned from GitHub
	Pull *gh.PullRequest
}

// getPRMetadata constructs the PRMetadata for the current state of the branch.
// TODO:
// The way we pass/load all the relevant data here is not great(tm). The
// `parent` argument is optional because sometimes it's loaded by the
// calling function and sometimes not. :shrug: It can also be nil if the
// branch doesn't have a parent (i.e., the branch is a stack root).
func getPRMetadata(
	tx meta.ReadTx,
	branch meta.Branch,
	parent *meta.Branch,
) (PRMetadata, error) {
	trunk, _ := meta.Trunk(tx, branch.Name)
	prMeta := PRMetadata{
		Parent:     branch.Parent.Name,
		ParentHead: branch.Parent.BranchingPointCommitHash,
		Trunk:      trunk,
	}
	if parent == nil && branch.Parent.Name != "" {
		p, _ := tx.Branch(branch.Parent.Name)
		parent = &p
	}
	if parent != nil && parent.PullRequest != nil {
		prMeta.ParentPull = parent.PullRequest.Number
	}
	return prMeta, nil
}

type errPullRequestClosed struct {
	*gh.PullRequest
}

func (e errPullRequestClosed) Error() string {
	return fmt.Sprintf("pull request #%d is %s", e.Number, e.State)
}

// getExistingOpenPR returns an existing pull request for the given branch if
// any exist and are open.
func getExistingOpenPR(
	ctx context.Context,
	client *gh.Client,
	repoMeta meta.Repository,
	branchMeta meta.Branch,
	baseRefName string,
) (*gh.PullRequest, error) {
	if branchMeta.PullRequest != nil {
		logrus.WithField("pr", branchMeta.PullRequest.Number).
			Debug("querying data for existing PR from GitHub")
		pr, err := client.PullRequest(ctx, branchMeta.PullRequest.ID)
		if err != nil {
			return nil, errors.WrapIf(err, "querying existing pull request")
		}
		if pr.State != githubv4.PullRequestStateOpen {
			return nil, errPullRequestClosed{pr}
		}
		return pr, nil
	}
	logrus.WithField("branch", branchMeta.Name).Debug("querying existing open PRs from GitHub")
	existing, err := client.GetPullRequests(ctx, gh.GetPullRequestsInput{
		Owner:       repoMeta.Owner,
		Repo:        repoMeta.Name,
		HeadRefName: branchMeta.Name,
		BaseRefName: baseRefName,
		States:      []githubv4.PullRequestState{githubv4.PullRequestStateOpen},
	})
	if err != nil {
		return nil, errors.WrapIf(err, "querying existing pull requests")
	}
	if len(existing.PullRequests) > 1 {
		return nil, errors.Errorf("multiple existing PRs found for %q", branchMeta.Name)
	} else if len(existing.PullRequests) == 1 {
		return &existing.PullRequests[0], nil
	}
	return nil, nil
}

// CreatePullRequest creates a pull request on GitHub for the current branch, if
// one doesn't already exist.
func CreatePullRequest(
	ctx context.Context,
	repo *git.Repo,
	client *gh.Client,
	tx meta.WriteTx,
	opts CreatePullRequestOpts,
) (_ *CreatePullRequestResult, reterr error) {
	if opts.BranchName == "" {
		logrus.Panicf("internal invariant error: CreatePullRequest called with empty branch name")
	}

	repoMeta := tx.Repository()
	branchMeta, _ := tx.Branch(opts.BranchName)

	var existingPR *gh.PullRequest
	if !opts.Force {
		var err error
		existingPR, err = getExistingOpenPR(ctx, client, repoMeta, branchMeta, opts.BranchName)
		if closed, ok := errutils.As[errPullRequestClosed](err); ok {
			_, _ = fmt.Fprint(
				os.Stderr,
				colors.Failure("Existing pull request for branch "),
				colors.UserInput(opts.BranchName),
				colors.Failure(" is "), colors.UserInput(closed.State),
				colors.Failure(": "), colors.UserInput(closed.Permalink),
				"\n",
			)
			_, _ = fmt.Fprint(
				os.Stderr,
				colors.Faint("  - use "), colors.CliCmd("av pr --force"),
				colors.Faint(" to create a new pull request for this branch\n"),
			)
			return nil, err
		} else if err != nil {
			return nil, errors.WrapIf(err, "failed to get existing pull request")
		}
	} else {
		// If we're forcing creation of a new PR, we always want to force push.
		// This prevents errors where we try to use `--force-with-lease` but the
		// remote branch has been deleted.
		opts.ForcePush = true
	}

	verb := "Creating"
	if existingPR != nil {
		verb = "Updating"
	}
	_, _ = fmt.Fprint(
		os.Stderr,
		verb, " pull request for branch ", colors.UserInput(opts.BranchName), ":",
		"\n",
	)
	if !opts.NoPush || opts.ForcePush {
		pushFlags := []string{"push"}

		if opts.ForcePush {
			pushFlags = append(pushFlags, "--force")
		} else {
			// Use --force-with-lease to allow pushing branches that have been
			// rebased but don't overwrite changes if we don't expect them to
			// be there.
			pushFlags = append(pushFlags, "--force-with-lease")
		}

		remote := repo.GetRemoteName()
		pushFlags = append(pushFlags, remote, opts.BranchName)
		logrus.Debug("pushing latest changes")

		_, _ = fmt.Fprint(
			os.Stderr,
			"  - pushing to ", color.CyanString("%s/%s", remote, opts.BranchName),
			"\n",
		)
		if _, err := repo.Git(ctx, pushFlags...); err != nil {
			return nil, errors.WrapIf(err, "failed to push")
		}
		if err := repo.BranchSetConfig(ctx, opts.BranchName, "av-pushed-remote", remote); err != nil {
			return nil, err
		}
		if err := repo.BranchSetConfig(ctx, opts.BranchName, "av-pushed-ref", fmt.Sprintf("refs/heads/%s", opts.BranchName)); err != nil {
			return nil, err
		}
		if err := repo.BranchSetUpstream(ctx, opts.BranchName, remote); err != nil {
			return nil, err
		}
	} else {
		_, _ = fmt.Fprint(
			os.Stderr,
			"  - skipping push to GitHub",
			"\n",
		)
	}

	// figure this out based on whether or not we're on a stacked branch
	parentState := branchMeta.Parent
	if parentState.Name == "" {
		defaultBranch := repo.DefaultBranch()
		parentState = meta.BranchState{
			Name:  defaultBranch,
			Trunk: true,
		}
		branchMeta.Parent = parentState
	}
	prCompareRef := parentState.Name
	var parentMeta meta.Branch
	if !parentState.Trunk {
		// check if the base branch also has an associated PR
		var ok bool
		parentMeta, ok = tx.Branch(parentState.Name)
		if !ok {
			return nil, errors.Errorf("failed to read branch metadata for %q", parentState.Name)
		}
		if parentMeta.PullRequest == nil {
			// TODO:
			//     We should automagically create PRs for every branch in the stack
			return nil, errors.Errorf(
				"base branch %q does not have an associated pull request "+
					"(create one by checking out the branch and running 'av pr')",
				parentState.Name,
			)
		}
	} else {
		logrus.WithField("base", parentState.Name).Debug("base branch is a trunk branch")
		prCompareRef = fmt.Sprintf("%s/%s", repo.GetRemoteName(), parentState.Name)
	}

	commitsList, err := repo.Git(
		ctx,
		"rev-list",
		"--reverse",
		fmt.Sprintf("%s..%s", prCompareRef, opts.BranchName),
		"--",
	)
	if err != nil {
		return nil, errors.WrapIf(err, "failed to determine commits to include in PR")
	}
	if commitsList == "" {
		return nil, errors.Errorf("no commits between %q and %q", prCompareRef, opts.BranchName)
	}

	// Check if a parent branch has already been merged or not
	if parentMeta.MergeCommit != "" {
		return nil, errors.Errorf(
			"failed to create a pull request. The parent branch %q has already been merged\nPlease run 'av sync' to rebase the branch first.",
			parentMeta.Name,
		)
	}

	if existingPR != nil {
		// If there's an existing PR, use that as the new PR title and body. If --edit is
		// used, an editor is opened later.
		if opts.Title == "" {
			opts.Title = existingPR.Title
		}
		if opts.Body == "" {
			// Not clear when this happens, but it seems that the body sometimes has
			// \r\n as line endings. Convert them to \n for consistency.
			body := strings.ReplaceAll(existingPR.Body, "\r\n", "\n")
			// Existing PR body may have metadata appended to it. Trim that off.
			if stripped, _, err := ParsePRBody(body); err == nil {
				body = stripped
			}
			opts.Body = body
		}
	}

	if opts.Edit || (opts.Body == "" && opts.Title == "") {
		var commits []git.CommitInfo
		for commitHash := range strings.SplitSeq(commitsList, "\n") {
			commit, err := repo.CommitInfo(ctx, git.CommitInfoOpts{Rev: commitHash})
			if err != nil {
				return nil, errors.WrapIff(err, "failed to get commit info for %q", commitHash)
			}
			commits = append(commits, *commit)
		}

		// If a saved pull request description exists, use that.
		saveFile := filepath.Join(
			repo.AvTmpDir(),
			fmt.Sprintf("av-pr-%s.md", sanitize.FileName(opts.BranchName)),
		)
		if _, err := os.Stat(saveFile); err == nil {
			contents, err := os.ReadFile(saveFile)
			if err != nil {
				logrus.WithError(err).Warn("failed to read saved pull request description")
			} else {
				title, body := stringutils.ParseSubjectBody(string(contents))
				if opts.Title == "" {
					opts.Title = title
				}
				if opts.Body == "" {
					opts.Body = body
				}
			}
		}

		// Try to populate the editor text using contextual information from the
		// repository and commits included in this pull request.
		if opts.Title == "" {
			opts.Title = commits[0].Subject
		}
		// Reasonable defaults for body:
		// 1. Try and find a pull request template
		if opts.Body == "" {
			opts.Body = readDefaultPullRequestTemplate(repo)
		}
		// 2. Use the commit message from the first PR
		if opts.Body == "" {
			opts.Body = commits[0].Body
		}

		editorText := templateutils.MustString(prBodyTemplate, prBodyTemplateData{
			Branch:  opts.BranchName,
			Title:   opts.Title,
			Body:    opts.Body,
			Commits: commits,
		})

		res, err := editor.Launch(ctx, repo, editor.Config{
			Text:           editorText,
			TmpFilePattern: "pr-*.av.md",
			CommentPrefix:  "%%",
		})
		if err != nil {
			if res != "" {
				savePRDescriptionToTemporaryFile(saveFile, res)
			}
			return nil, errors.WrapIf(err, "text editor failed")
		}
		opts.Title, opts.Body = stringutils.ParseSubjectBody(res)
		// The tailing new line is needed for compare with `PRMetadataCommentEnd` during the metadata parsing.
		opts.Body += "\n"

		defer func() {
			// If we created the PR successfully, just make sure to clean up any
			// lingering files.
			if reterr == nil {
				_ = os.Remove(saveFile)
				return
			}

			// Otherwise, save what the user entered to a file so that it's not
			// lost forever (and we can reuse it if they try again).
			savePRDescriptionToTemporaryFile(saveFile, res)
		}()
	}
	if opts.Title == "" {
		return nil, errors.New("aborting pull request due to empty message")
	}

	prMeta, err := getPRMetadata(tx, branchMeta, &parentMeta)
	if err != nil {
		return nil, err
	}

	draft := opts.Draft
	if !config.Av.PullRequest.NoWIPDetection && strings.Contains(opts.Title, "WIP") {
		draft = true
	}

	pull, didCreatePR, err := ensurePR(ctx, client, repoMeta, tx, ensurePROpts{
		baseRefName: parentState.Name,
		headRefName: opts.BranchName,
		title:       opts.Title,
		body:        opts.Body,
		meta:        prMeta,
		draft:       draft,
		existingPR:  existingPR,
	})
	if err != nil {
		_, _ = fmt.Fprint(
			os.Stderr,
			colors.Failure("  - failed to create pull request: "), err, "\n",
		)
		return nil, errors.WrapIf(err, "failed to create PR")
	}

	branchMeta.PullRequest = &meta.PullRequest{
		Number:    pull.Number,
		ID:        pull.ID,
		Permalink: pull.Permalink,
	}
	// It's possible that a new PR is created with the same branch. Reset the MergeCommit.
	branchMeta.MergeCommit = ""
	var action string
	if didCreatePR {
		action = "created"
	} else {
		action = "synchronized"
	}
	_, _ = fmt.Fprint(
		os.Stderr,
		"  - ", action, " pull request ",
		colors.UserInput(pull.Permalink), "\n",
	)

	if didCreatePR && !opts.NoOpenBrowser && config.Av.PullRequest.OpenBrowser {
		OpenPullRequestInBrowser(ctx, pull.Permalink)
	}

	tx.SetBranch(branchMeta)
	return &CreatePullRequestResult{didCreatePR, branchMeta, pull}, nil
}

func OpenPullRequestInBrowser(ctx context.Context, pullRequestLink string) {
	if err := browser.Open(ctx, pullRequestLink); err != nil {
		_, _ = fmt.Fprint(
			os.Stderr,
			"  - couldn't open browser ",
			colors.UserInput(err),
			" for pull request link ",
			colors.UserInput(pullRequestLink),
		)
	}
}

func savePRDescriptionToTemporaryFile(saveFile string, contents string) {
	if err := os.WriteFile(saveFile, []byte(contents), 0o644); err != nil {
		logrus.WithError(err).
			Error("failed to write pull request description to temporary file")
		return
	}
	_, _ = fmt.Fprint(
		os.Stderr,
		"  - saved pull request description to ", colors.UserInput(saveFile),
		" (it will be automatically re-used if you try again)\n",
	)
}

type prBodyTemplateData struct {
	Branch  string
	Title   string
	Body    string
	Commits []git.CommitInfo
}

var templateFuncs = template.FuncMap{"trimSpace": strings.TrimSpace}

var prBodyTemplate = template.Must(
	template.New("prBody").
		Funcs(templateFuncs).
		Parse(`%% Creating pull request for branch '{{ .Branch }}'
%% Lines starting with '%%' will be ignored and an empty message aborts the
%% creation of the pull request.

%% Pull request title (single line)
{{ .Title }}

%% Pull request body (multiple lines)
{{ .Body }}

%% This branch includes the following commits:
{{- range $c := .Commits }}
%%     {{ $c.ShortHash }}    {{ $c.Subject }}
{{- if trimSpace $c.Body }}
{{- range $line := $c.BodyWithPrefix "%%         " }}
{{ trimSpace $line }}
{{- end }}
{{- end }}
{{- end }}
`),
)

func readDefaultPullRequestTemplate(repo *git.Repo) string {
	for _, dir := range []string{"", ".github", "data"} {
		for _, f := range []string{
			"PULL_REQUEST_TEMPLATE.md",
			"pull_request_template.md",
		} {
			tpl := filepath.Join(repo.Dir(), dir, f)
			data, err := os.ReadFile(tpl)
			if err != nil {
				continue
			}
			return string(data)
		}
	}
	return ""
}

type ensurePROpts struct {
	baseRefName string
	headRefName string
	title       string
	body        string
	meta        PRMetadata
	draft       bool
	existingPR  *gh.PullRequest
}

// ensurePR returns the pull request for the given input, creating a new
// pull request if one doesn't exist. It returns the pull request, a boolean
// indicating whether or not the pull request was created, and an error if one
// occurred.
func ensurePR(
	ctx context.Context,
	client *gh.Client,
	repoMeta meta.Repository,
	tx meta.ReadTx,
	opts ensurePROpts,
) (*gh.PullRequest, bool, error) {
	// Don't pass in a stack to start; we'll do a pass over all open PRs in the stack later.
	var initialStack *stackutils.StackTreeNode = nil

	if opts.existingPR != nil {
		newBody := AddPRMetadataAndStack(opts.body, opts.meta, opts.headRefName, initialStack, tx, nil, nil)
		updatedPR, err := client.UpdatePullRequestIfChanged(ctx, opts.existingPR, gh.UpdatePullRequestFields{
			Title:       gh.Ptr(opts.title),
			Body:        gh.Ptr(newBody),
			BaseRefName: gh.Ptr(opts.baseRefName),
		})
		if err != nil {
			return nil, false, errors.WithStack(err)
		}
		return updatedPR, false, nil
	}
	pull, err := client.CreatePullRequest(ctx, githubv4.CreatePullRequestInput{
		RepositoryID: githubv4.ID(repoMeta.ID),
		BaseRefName:  githubv4.String(opts.baseRefName),
		HeadRefName:  githubv4.String(opts.headRefName),
		Title:        githubv4.String(opts.title),
		Body: gh.Ptr(
			githubv4.String(
				AddPRMetadataAndStack(opts.body, opts.meta, opts.headRefName, initialStack, tx, nil, nil),
			),
		),
		Draft: gh.Ptr(githubv4.Boolean(opts.draft)),
	})
	if err != nil {
		return nil, false, errors.WithStack(err)
	}
	return pull, true, nil
}

type UpdatePullRequestResult struct {
	// True if the pull request information changed (e.g., a new pull request
	// was found or if the pull request changed state)
	Changed bool
	// The pull request object that was returned from GitHub
	Pull *gh.PullRequest
}

// UpdatePullRequestState fetches the latest pull request information from GitHub
// and writes the relevant branch metadata.
func UpdatePullRequestState(
	ctx context.Context,
	client *gh.Client,
	tx meta.WriteTx,
	branchName string,
) (*UpdatePullRequestResult, error) {
	repoMeta := tx.Repository()
	branch, _ := tx.Branch(branchName)

	page, err := client.GetPullRequests(ctx, gh.GetPullRequestsInput{
		Owner:       repoMeta.Owner,
		Repo:        repoMeta.Name,
		HeadRefName: branchName,
	})
	if err != nil {
		return nil, errors.WrapIf(
			err,
			"querying GitHub pull requests. Make sure GitHub token is set or refresh.\nSee: https://docs.aviator.co/aviator-cli#getting-started",
		)
	}

	if len(page.PullRequests) == 0 {
		// branch has no pull request
		if branch.PullRequest != nil {
			// This should never happen?
			logrus.WithFields(logrus.Fields{
				"branch": branch.Name,
				"pull":   branch.PullRequest.Permalink,
			}).Error("GitHub reported no pull requests for branch but local metadata has pull request")
			return nil, errors.New(
				"GitHub reported no pull requests for branch but local metadata has pull request",
			)
		}

		return &UpdatePullRequestResult{false, nil}, nil
	}

	// The latest info for the pull request that we have stored in local metadata
	// (we can use this to check if the pull was closed/merged)
	var currentPull *gh.PullRequest
	// The current open pull request (if any)
	var openPull *gh.PullRequest
	for i := range page.PullRequests {
		pull := &page.PullRequests[i]
		if branch.PullRequest != nil && pull.ID == branch.PullRequest.ID {
			currentPull = pull
		}
		if pull.State != githubv4.PullRequestStateOpen {
			continue
		}
		// GH only allows one open pull for a given (head, base) pair, but
		// we only support one open pull per head branch (the workflow of
		// opening a pull from a head branch into multiple base branches is
		// rare). This probably isn't necessary but better to be defensive
		// here.
		if openPull != nil {
			return nil, errors.Errorf(
				"multiple open pull requests for branch %q (#%d into %q and #%d into %q)",
				branchName,
				openPull.Number, openPull.BaseRefName,
				pull.Number, pull.BaseRefName,
			)
		}
		openPull = pull
	}

	changed := false
	var oldId string
	if branch.PullRequest != nil {
		oldId = branch.PullRequest.ID
	}

	var newPull *gh.PullRequest
	if openPull != nil {
		if oldId != openPull.ID {
			changed = true
		}
		branch.PullRequest = &meta.PullRequest{
			ID:        openPull.ID,
			Number:    openPull.Number,
			Permalink: openPull.Permalink,
			State:     openPull.State,
		}
		newPull = openPull
	} else {
		// openPull is nil so the PR should be merged or closed
		if currentPull != nil {
			// Only set merge commit if the PR is actually in MERGED state.
			// PRs can be marked as "merged" by GitHub when they have no diff
			// (e.g., when changes are flattened into parent), but they haven't
			// actually been merged into trunk. The fetch logic will verify if
			// the merge commit is in trunk history before propagating.
			if currentPull.State == githubv4.PullRequestStateMerged {
				branch.MergeCommit = currentPull.GetMergeCommit()
			}
			branch.PullRequest = &meta.PullRequest{
				ID:        currentPull.ID,
				Number:    currentPull.Number,
				Permalink: currentPull.Permalink,
				State:     currentPull.State,
			}
		} else {
			// openPull and currentPull is nil
			if branch.PullRequest != nil {
				changed = true
			}
			branch.PullRequest = nil
		}
		newPull = currentPull
	}

	tx.SetBranch(branch)
	return &UpdatePullRequestResult{changed, newPull}, nil
}

type PRMetadata struct {
	Parent     string `json:"parent"`
	ParentHead string `json:"parentHead"`
	ParentPull int64  `json:"parentPull,omitempty"`
	Trunk      string `json:"trunk"`
}

const PRMetadataCommentStart = "<!-- av pr metadata"

const (
	PRMetadataCommentHelpText = "This information is embedded by the av CLI when creating PRs to track the status of stacks when using Aviator. Please do not delete or edit this section of the PR.\n"
	PRMetadataCommentEnd      = "-->"
)

const (
	PRStackCommentStart = "<!-- av pr stack begin -->"
	PRStackCommentEnd   = "<!-- av pr stack end -->"
)

// extractContent parses the given input and looks for the start and end
// strings. It returns the content between the start and end strings and the
// remaining input. If the start or end strings are not found, the content is
// empty and the input is returned as-is.
func extractContent(input string, start string, end string) (content string, output string) {
	startIndex := strings.Index(input, start)
	if startIndex == -1 {
		return "", input
	}
	contentIndex := startIndex + len(start)
	endIndex := strings.Index(input[contentIndex:], end)
	if endIndex == -1 {
		return "", input
	}

	content = strings.TrimSpace(input[contentIndex : contentIndex+endIndex])
	preContent := strings.TrimSpace(input[:startIndex])
	postContent := strings.TrimSpace(input[contentIndex+endIndex+len(end):])
	output = preContent
	if postContent != "" {
		output += "\n" + postContent
	}
	return content, output
}

func ParsePRBody(input string) (body string, prMeta PRMetadata, retErr error) {
	metadata, body := extractContent(input, PRMetadataCommentStart, PRMetadataCommentEnd)
	metadataContent, _ := extractContent(metadata, "```", "```")
	if metadataContent == "" {
		retErr = ErrNoPRMetadata
		return body, prMeta, retErr
	}
	if err := json.Unmarshal([]byte(metadataContent), &prMeta); err != nil {
		retErr = errors.WrapIff(err, "decoding PR metadata")
		return body, prMeta, retErr
	}

	_, body = extractContent(body, PRStackCommentStart, PRStackCommentEnd)

	return body, prMeta, retErr
}

func ReadPRMetadata(body string) (PRMetadata, error) {
	_, prMeta, err := ParsePRBody(body)
	return prMeta, err
}

// LineStat is a count of added/deleted lines.
type LineStat struct {
	Additions int
	Deletions int
}

// FileLineStat is a LineStat for a single file, identified by its repository-
// relative path (post-rename, matching the GitHub Files-changed anchor).
type FileLineStat struct {
	Path string
	LineStat
}

// CategoryLineStat is a LineStat for a single configured config.DiffStatCategory,
// along with the per-file breakdown that contributed to it (sorted by total
// changed lines, descending).
type CategoryLineStat struct {
	Name string
	LineStat
	Files []FileLineStat
}

// resolveDiffBase returns the ref to diff against for a parent branch.
//
// A trunk branch diffs against its remote tracking ref (e.g. origin/main),
// which is fresher than the local trunk that developers rarely keep up to
// date. A stacked (non-trunk) branch diffs against the local branch instead:
// av rebases it locally before pushing, so origin/<branch> can trail the
// local branch and inflate the counts.
func resolveDiffBase(ctx context.Context, repo *git.Repo, branch string) string {
	if slices.Contains(repo.TrunkBranches(), branch) {
		if ok, err := repo.DoesRemoteBranchExist(ctx, branch); err == nil && ok {
			return repo.GetRemoteName() + "/" + branch
		}
		return branch
	}
	if ok, err := repo.DoesBranchExist(ctx, branch); err == nil && ok {
		return branch
	}
	if ok, err := repo.DoesRemoteBranchExist(ctx, branch); err == nil && ok {
		return repo.GetRemoteName() + "/" + branch
	}
	return branch
}

// diffBase returns the commit to diff head against, matching GitHub's PR
// "Files changed" semantics: the merge-base of the parent branch and head.
// Diffing head against its parent's tip (a plain two-dot diff) would also
// surface parent-side changes that landed after the branch point — e.g. a
// trunk that advanced past the merge-base — inflating the counts.
func diffBase(ctx context.Context, repo *git.Repo, parentBranch, head string) (string, error) {
	return repo.MergeBase(ctx, resolveDiffBase(ctx, repo, parentBranch), head)
}

// ComputeStackDiffStats computes each branch's own LineStat (relative to its
// immediate parent in the stack) for every branch in stack.
func ComputeStackDiffStats(
	ctx context.Context,
	repo *git.Repo,
	stack *stackutils.StackTreeNode,
) (map[string]LineStat, error) {
	stats := make(map[string]LineStat)
	var visit func(node *stackutils.StackTreeNode) error
	visit = func(node *stackutils.StackTreeNode) error {
		// The root of the tree is the trunk branch, which has no parent within
		// the stack and isn't itself a PR.
		if node.Branch.ParentBranchName != "" {
			base, err := diffBase(ctx, repo, node.Branch.ParentBranchName, node.Branch.BranchName)
			if err != nil {
				return err
			}
			files, err := repo.DiffNumstat(ctx, base, node.Branch.BranchName)
			if err != nil {
				return err
			}
			var stat LineStat
			for _, f := range files {
				stat.Additions += f.Additions
				stat.Deletions += f.Deletions
			}
			stats[node.Branch.BranchName] = stat
		}
		for _, child := range node.Children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(stack); err != nil {
		return nil, err
	}
	return stats, nil
}

// ComputeCategoryDiffStats breaks down the diff between parentBranch and
// branchName into the configured categories, based on which file patterns
// each changed file matches. A file counts towards the first category (in
// config order) whose glob matches. Files that match none count towards a
// final fallback category named defaultCategoryName (omitted when empty).
// Categories with zero line changes are omitted from the result.
func ComputeCategoryDiffStats(
	ctx context.Context,
	repo *git.Repo,
	parentBranch, branchName string,
	categories []config.DiffStatCategory,
	defaultCategoryName string,
) ([]CategoryLineStat, error) {
	if len(categories) == 0 {
		return nil, nil
	}
	base, err := diffBase(ctx, repo, parentBranch, branchName)
	if err != nil {
		return nil, err
	}
	files, err := repo.DiffNumstat(ctx, base, branchName)
	if err != nil {
		return nil, err
	}

	type compiledCategory struct {
		name  string
		globs []glob.Glob
	}
	compiled := make([]compiledCategory, len(categories))
	for i, cat := range categories {
		compiled[i].name = cat.Name
		for _, pattern := range cat.Globs {
			g, err := glob.Compile(pattern, '/')
			if err != nil {
				logrus.WithError(err).
					WithField("pattern", pattern).
					Warn("ignoring invalid diffStatCategories glob pattern")
				continue
			}
			compiled[i].globs = append(compiled[i].globs, g)
		}
	}

	categoryFiles := make([][]FileLineStat, len(categories))
	var fallbackFiles []FileLineStat
	for _, f := range files {
		fls := FileLineStat{Path: f.Path, LineStat: LineStat{Additions: f.Additions, Deletions: f.Deletions}}
		assigned := false
		for i, cc := range compiled {
			for _, g := range cc.globs {
				if g.Match(f.Path) {
					categoryFiles[i] = append(categoryFiles[i], fls)
					assigned = true
					break
				}
			}
			if assigned {
				break
			}
		}
		if !assigned {
			fallbackFiles = append(fallbackFiles, fls)
		}
	}

	result := make([]CategoryLineStat, 0, len(categories)+1)
	for i, cc := range compiled {
		var stat LineStat
		for _, fl := range categoryFiles[i] {
			stat.Additions += fl.Additions
			stat.Deletions += fl.Deletions
		}
		if stat.Additions == 0 && stat.Deletions == 0 {
			continue
		}
		sortFilesDesc(categoryFiles[i])
		result = append(result, CategoryLineStat{Name: cc.name, LineStat: stat, Files: categoryFiles[i]})
	}
	if defaultCategoryName != "" {
		var stat LineStat
		for _, fl := range fallbackFiles {
			stat.Additions += fl.Additions
			stat.Deletions += fl.Deletions
		}
		if stat.Additions != 0 || stat.Deletions != 0 {
			sortFilesDesc(fallbackFiles)
			result = append(result, CategoryLineStat{Name: defaultCategoryName, LineStat: stat, Files: fallbackFiles})
		}
	}
	return result, nil
}

func walkStack(
	tx meta.ReadTx,
	stack *stackutils.StackTreeNode,
	branchName string,
	diffStats map[string]LineStat,
) string {
	ssb := strings.Builder{}

	// For simple stacks (i.e., degenerate trees) print them top-down. For example:
	// - #1
	// - #2
	// - main
	var visitSimple func(node *stackutils.StackTreeNode, depth int)
	visitSimple = func(node *stackutils.StackTreeNode, depth int) {
		bi, _ := tx.Branch(node.Branch.BranchName)
		if len(node.Children) > 1 {
			panic("stack tree has more than one child")
		} else if len(node.Children) == 1 {
			visitSimple(node.Children[0], depth+1)
		}

		ssb.WriteString("* ")

		if depth == 0 || bi.PullRequest == nil {
			ssb.WriteString("`")
			ssb.WriteString(node.Branch.BranchName)
			ssb.WriteString("`")
		} else {
			if node.Branch.BranchName == branchName {
				ssb.WriteString("➡️ ")
			}
			ssb.WriteString("**#")
			ssb.WriteString(strconv.FormatInt(bi.PullRequest.Number, 10))
			ssb.WriteString("**")
			if stat, ok := diffStats[node.Branch.BranchName]; ok {
				ssb.WriteString(fmt.Sprintf(" (+%d -%d)", stat.Additions, stat.Deletions))
			}
		}
		ssb.WriteString("\n")
	}

	// For more complex stacks, print them sideways using a bulleted list. For example:
	// - main
	//   - #1
	//     - #2
	//   - #3
	var visitComplex func(node *stackutils.StackTreeNode, depth int)
	visitComplex = func(node *stackutils.StackTreeNode, depth int) {
		if depth == 0 {
			ssb.WriteString("* ")
			ssb.WriteString("`")
			ssb.WriteString(node.Branch.BranchName)
			ssb.WriteString("`")
		} else {
			bi, _ := tx.Branch(node.Branch.BranchName)
			ssb.WriteString(strings.Repeat("  ", depth))
			ssb.WriteString("* ")
			if node.Branch.BranchName == branchName {
				ssb.WriteString("➡️ ")
			}
			if bi.PullRequest != nil {
				ssb.WriteString("**#")
				ssb.WriteString(strconv.FormatInt(bi.PullRequest.Number, 10))
				ssb.WriteString("**")
				if stat, ok := diffStats[node.Branch.BranchName]; ok {
					ssb.WriteString(fmt.Sprintf(" (+%d -%d)", stat.Additions, stat.Deletions))
				}
			} else {
				ssb.WriteString("`")
				ssb.WriteString(node.Branch.BranchName)
				ssb.WriteString("`")
			}
		}
		ssb.WriteString("\n")

		for _, child := range node.Children {
			visitComplex(child, depth+1)
		}
	}

	var hasMultipleChildren func(node *stackutils.StackTreeNode) bool
	hasMultipleChildren = func(node *stackutils.StackTreeNode) bool {
		if len(node.Children) > 1 {
			return true
		} else if len(node.Children) == 1 {
			return hasMultipleChildren(node.Children[0])
		}
		return false
	}

	// Optimize navigation within a stack by making sure the output has the same shape everywhere.
	if hasMultipleChildren(stack) {
		visitComplex(stack, 0)
	} else {
		visitSimple(stack, 0)
	}

	return ssb.String()
}

// diffStatBarWidth is the number of characters in a proportional bar.
const diffStatBarWidth = 10

// maxFilenameLength is the maximum rendered length of a file path in the
// breakdown; longer paths are truncated with a middle ellipsis so the leading
// directories and trailing filename/extension both stay visible.
const maxFilenameLength = 36

// childIndent leads each file row under its category.
const childIndent = "\u00a0\u00a0\u00a0\u00a0"

// sortFilesDesc sorts files in place by total changed lines, descending.
func sortFilesDesc(files []FileLineStat) {
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].Additions+files[i].Deletions > files[j].Additions+files[j].Deletions
	})
}

// barAndPercent returns the proportional bar (relative to max) and the absolute
// percentage (share of sum) for a total.
func barAndPercent(total, max, sum int) (string, int) {
	filled := (total*diffStatBarWidth + max/2) / max
	if filled < 1 {
		filled = 1
	}
	if filled > diffStatBarWidth {
		filled = diffStatBarWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", diffStatBarWidth-filled)
	percentage := (total*100 + sum/2) / sum
	return bar, percentage
}

// truncateMiddle shortens s to at most max runes, replacing the cut with a
// middle ellipsis so the tail (filename and extension) stays visible.
func truncateMiddle(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max < 2 {
		return string(runes[:max])
	}
	head := (max - 1) / 2
	tail := max - 1 - head
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

// markdownFileLink returns a Markdown link to the file's diff in the PR's
// Files-changed view (truncating the displayed path), or the plain full path
// when no permalink is available. The anchor fragment is the SHA-256 of the
// repository-relative path, matching GitHub's own #diff-<hash> anchors.
func markdownFileLink(path, prPermalink string) string {
	if prPermalink == "" {
		return path
	}
	display := truncateMiddle(path, maxFilenameLength)
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("[%s](%s/files#diff-%s)", display, prPermalink, hex.EncodeToString(sum[:]))
}

// renderDiffStatBarChart renders two tables: an always-visible category rollup
// (bar relative to the largest category, percentage of the PR total), followed
// by a collapse-by-default "Detailed Breakdown" that relists each category (no
// changes/bar column) with its files indented beneath (bar relative to the
// category's largest file, percentage of the category's total). File paths are
// truncated with a middle ellipsis.
func renderDiffStatBarChart(stats []CategoryLineStat, prPermalink string) string {
	sorted := append([]CategoryLineStat(nil), stats...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Additions+sorted[i].Deletions > sorted[j].Additions+sorted[j].Deletions
	})
	if len(sorted) == 0 {
		return ""
	}
	maxTotal := 0
	sumTotal := 0
	for _, s := range sorted {
		total := s.Additions + s.Deletions
		sumTotal += total
		if total > maxTotal {
			maxTotal = total
		}
	}
	if sumTotal == 0 {
		return ""
	}

	sb := strings.Builder{}
	sb.WriteString("### PR Breakdown\n\n")
	sb.WriteString("| Breakdown | Changes | |\n")
	sb.WriteString("|:--|--:|---|\n")
	for _, s := range sorted {
		bar, percentage := barAndPercent(s.Additions+s.Deletions, maxTotal, sumTotal)
		lines := fmt.Sprintf("+%s -%s",
			humanize.Comma(int64(s.Additions)),
			humanize.Comma(int64(s.Deletions)),
		)
		sb.WriteString(fmt.Sprintf("| **%s** | `%s` | `%s %d%%` |\n", s.Name, lines, bar, percentage))
	}

	var anyFiles bool
	for _, s := range sorted {
		if len(s.Files) > 0 {
			anyFiles = true
			break
		}
	}
	if !anyFiles {
		return sb.String()
	}

	sb.WriteString("\n<details><summary>Detailed Breakdown</summary>\n\n")
	sb.WriteString("| Breakdown | Changes | |\n")
	sb.WriteString("|:--|--:|---|\n")
	for _, s := range sorted {
		if len(s.Files) == 0 {
			continue
		}
		label := "files"
		if len(s.Files) == 1 {
			label = "file"
		}
		sb.WriteString(fmt.Sprintf("| **%s** | %d %s | |\n", s.Name, len(s.Files), label))
		files := append([]FileLineStat(nil), s.Files...)
		sortFilesDesc(files)
		catTotal := s.Additions + s.Deletions
		maxFileTotal := files[0].Additions + files[0].Deletions
		for _, f := range files {
			fbar, fpct := barAndPercent(f.Additions+f.Deletions, maxFileTotal, catTotal)
			flines := fmt.Sprintf("+%s -%s",
				humanize.Comma(int64(f.Additions)),
				humanize.Comma(int64(f.Deletions)),
			)
			sb.WriteString(fmt.Sprintf("| %s%s | `%s` | `%s %d%%` |\n",
				childIndent, markdownFileLink(f.Path, prPermalink), flines, fbar, fpct))
		}
	}
	sb.WriteString("\n</details>\n")
	return sb.String()
}

func AddPRMetadataAndStack(
	body string,
	prMeta PRMetadata,
	branchName string,
	stack *stackutils.StackTreeNode,
	tx meta.ReadTx,
	diffStats map[string]LineStat,
	categoryStats []CategoryLineStat,
) string {
	body, _, err := ParsePRBody(body)
	if err != nil {
		// No existing metadata comment, so add one.
		logrus.WithError(err).Debug("could not parse PR metadata (assuming it doesn't exist)")
	}

	sb := strings.Builder{}

	hasMultilevelStack := stack != nil && len(stack.Children) > 0 &&
		len(stack.Children[0].Children) > 0
	hasBreakdown := len(categoryStats) > 0
	if hasMultilevelStack || hasBreakdown {
		sb.WriteString(PRStackCommentStart)

		// Wrap the entire stack comment in a single 1x1 table so it renders as
		// one bordered box. Slack strips <table> in unfurls, so none of this
		// shows there.
		sb.WriteString("\n<table><tr><td>\n\n")
		if hasMultilevelStack {
			bi, _ := tx.Branch(branchName)
			stackString := walkStack(tx, stack, branchName, diffStats)

			sb.WriteString("<details><summary>")
			if !bi.Parent.Trunk {
				parentBi, _ := tx.Branch(bi.Parent.Name)
				sb.WriteString("<b>Depends on #")
				sb.WriteString(strconv.FormatInt(parentBi.PullRequest.Number, 10))
				sb.WriteString(".</b> ")
			}
			sb.WriteString(
				"This PR is part of a stack created with <a href=\"https://github.com/aviator-co/av\">Aviator</a>.",
			)
			sb.WriteString("</summary>")
			sb.WriteString("\n\n")
			sb.WriteString(stackString)
			sb.WriteString("</details>\n\n")
		}
		if hasBreakdown {
			prPermalink := ""
			if bi, ok := tx.Branch(branchName); ok && bi.PullRequest != nil {
				prPermalink = bi.PullRequest.Permalink
			}
			sb.WriteString(renderDiffStatBarChart(categoryStats, prPermalink))
			sb.WriteString("\n")
		}
		sb.WriteString("</td></tr></table>\n")
		sb.WriteString(PRStackCommentEnd)
		sb.WriteString("\n\n")
	}

	sb.WriteString(body)

	sb.WriteString("\n\n")
	sb.WriteString(PRMetadataCommentStart)
	sb.WriteString("\n")
	sb.WriteString(PRMetadataCommentHelpText)
	sb.WriteString("```\n")

	// Note: Encoder.Encode implicitly adds a newline at the end of the JSON
	// which is important here so that the ``` below appears on its own line.
	if err := json.NewEncoder(&sb).Encode(prMeta); err != nil {
		// shouldn't ever happen since we're encoding a simple struct to a buffer
		panic(errors.WrapIff(err, "encoding PR metadata"))
	}
	sb.WriteString("```\n")
	sb.WriteString(PRMetadataCommentEnd)
	sb.WriteString("\n")

	return sb.String()
}

// UpdatePullRequestWithStack updates the GitHub pull request associated with the given branch to include
// the stack of branches that the branch is a part of.
// This should be called after all applicable PRs have been created to ensure we can properly link them.
func UpdatePullRequestWithStack(
	ctx context.Context,
	repo *git.Repo,
	client *gh.Client,
	tx meta.WriteTx,
	branchName string,
) error {
	branchMeta, exists := tx.Branch(branchName)
	// it's possible that this branch is not part of the primary stack, ex: they have main->branchA->branchB,
	// but forkA is coming from branchA in which case forkA may not have a pull request associated with it.
	// In this case, we should not try to update forkA's pull request with the stack.
	if !exists || branchMeta.PullRequest == nil {
		return nil
	}
	logrus.WithField("branch", branchName).
		WithField("pr", branchMeta.PullRequest.ID).
		Debug("Updating pull requests with stack")

	repoMeta := tx.Repository()

	// Don't sort based on the current branch so that the output is consistent between branches.
	stackToWrite, err := stackutils.BuildStackTreeCurrentStack(tx, branchName, false)
	if err != nil {
		return err
	}

	var diffStats map[string]LineStat
	if config.Av.PullRequest.ShowStackDiffStat {
		if diffStats, err = ComputeStackDiffStats(ctx, repo, stackToWrite); err != nil {
			return err
		}
	}
	var categoryStats []CategoryLineStat
	if len(config.Av.PullRequest.DiffStatCategories) > 0 {
		if categoryStats, err = ComputeCategoryDiffStats(
			ctx, repo, branchMeta.Parent.Name, branchName, config.Av.PullRequest.DiffStatCategories,
			config.Av.PullRequest.DiffStatDefaultCategory,
		); err != nil {
			return err
		}
	}

	existingPR, err := getExistingOpenPR(ctx, client, repoMeta, branchMeta, branchName)
	if err != nil {
		return errors.WithStack(err)
	}

	body, prMeta, err := ParsePRBody(existingPR.Body)
	if err != nil {
		return err
	}

	newBody := AddPRMetadataAndStack(body, prMeta, branchName, stackToWrite, tx, diffStats, categoryStats)
	if _, err := client.UpdatePullRequestIfChanged(ctx, existingPR, gh.UpdatePullRequestFields{
		Body: gh.Ptr(newBody),
	}); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// UpdatePullRequestsWithStack updates the GitHub pull requests associated with the given branches to include
// the stack of branches that each branch is a part of.
func UpdatePullRequestsWithStack(
	ctx context.Context,
	repo *git.Repo,
	client *gh.Client,
	tx meta.WriteTx,
	branchNames []string,
) error {
	for _, branchName := range branchNames {
		if err := UpdatePullRequestWithStack(ctx, repo, client, tx, branchName); err != nil {
			return err
		}
	}

	return nil
}
