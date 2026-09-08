package git

import (
	"context"
	"strconv"
	"strings"

	"emperror.dev/errors"
)

type DiffOpts struct {
	// The revisions to compare.
	// The behavior of the diff changes depending on how these are specified.
	//   - If empty, the generated diff is relative to the current staging area.
	//   - If one commit is given, the diff is calculated between the working tree
	//     and the given commit.
	//   - If two commits are given (or one string representing a commit range
	//     like `<a>..<b>`), the diff is calculated between the two commits.
	Specifiers []string
	// If true, don't actually generate the diff, just return whether or not its
	// empty. If set, Diff.Contents will always be an empty string.
	Quiet bool
	// If true, shows the colored diff.
	Color bool
	// If specified, compare only the specified paths.
	Paths []string
}

type Diff struct {
	// If true, there are no differences between the working tree and the commit.
	Empty    bool
	Contents string
}

func (r *Repo) Diff(ctx context.Context, d *DiffOpts) (*Diff, error) {
	args := []string{"diff", "--exit-code"}
	if d.Quiet {
		args = append(args, "--quiet")
	}
	if d.Color {
		args = append(args, "--color=always")
	}

	args = append(args, d.Specifiers...)

	// This needs to be last because everything after the `--` is interpreted
	// as a path, not a flag.
	// Note that we still append this `--` even if there are no paths because
	// otherwise Git might interpret a specifier as ambiguous path and raise an
	// error.
	args = append(args, "--")
	args = append(args, d.Paths...)

	output, err := r.Run(ctx, &RunOpts{
		Args: args,
	})
	if err != nil {
		return nil, err
	}
	if output.ExitCode == 1 {
		return &Diff{Empty: false, Contents: string(output.Stdout)}, nil
	} else if output.ExitCode != 0 {
		return nil, errors.Errorf("git diff failed: %s", string(output.Stderr))
	}
	return &Diff{Empty: true, Contents: string(output.Stdout)}, nil
}

// FileDiffStat is the added/deleted line count for a single file in a diff.
type FileDiffStat struct {
	// The file path, relative to the repository root. For renames, this is the
	// path of the file after the rename.
	Path      string
	Additions int
	Deletions int
}

// DiffNumstat returns the per-file added/deleted line counts between two
// revisions (equivalent to `git diff --numstat base head`).
func (r *Repo) DiffNumstat(ctx context.Context, base, head string) ([]FileDiffStat, error) {
	output, err := r.Run(ctx, &RunOpts{
		Args: []string{"diff", "--numstat", base, head},
	})
	if err != nil {
		return nil, err
	}
	if output.ExitCode != 0 {
		return nil, errors.Errorf("git diff --numstat failed: %s", string(output.Stderr))
	}

	lines := strings.Split(strings.TrimRight(string(output.Stdout), "\n"), "\n")
	stats := make([]FileDiffStat, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		added, deleted, path := fields[0], fields[1], fields[2]

		// Renamed files are reported as "old => new", or with a "{old => new}"
		// common-prefix shorthand (e.g. "common/{old => new}/file.txt"); take
		// the post-rename path in both cases.
		if open := strings.Index(path, "{"); open != -1 {
			if closeIdx := strings.Index(path[open:], "}"); closeIdx != -1 {
				closeIdx += open
				if parts := strings.SplitN(path[open+1:closeIdx], "=>", 2); len(parts) == 2 {
					path = path[:open] + strings.TrimSpace(parts[1]) + path[closeIdx+1:]
				}
			}
		} else if idx := strings.Index(path, "=>"); idx != -1 {
			path = strings.TrimSpace(path[idx+len("=>"):])
		}

		stat := FileDiffStat{Path: path}
		// Binary files are reported as "-\t-\tpath"; leave their counts at zero.
		if added != "-" {
			stat.Additions, _ = strconv.Atoi(added)
		}
		if deleted != "-" {
			stat.Deletions, _ = strconv.Atoi(deleted)
		}
		stats = append(stats, stat)
	}
	return stats, nil
}
