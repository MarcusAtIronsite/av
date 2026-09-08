# Fork modifications

This repository is a fork of [aviator-co/av](https://github.com/aviator-co/av) that carries a
set of local modifications on top of upstream `master`. This file is the catalog of those
modifications, so each divergence is enumerable and traceable independently of the others.

When you introduce a modification to this fork, add an entry to the table below in the same
change. When a modification is rebased onto a newer upstream, accepted upstream, replaced, or
dropped, update or remove its entry. `AGENTS.md` points here; treat the two as the source of
truth for what this fork changes.

| # | Modification | Files | Purpose | Upstream | Depends on |
|---|--------------|-------|---------|----------|------------|
| 1 | `DiffNumstat` helper with rename parsing | `internal/git/diff.go`, `internal/git/diff_test.go` | Adds a per-file line-count helper (`git diff --numstat`) and parses `old => new` and `{old => new}` rename paths. | Optional; self-contained, no upstream caller | — |
| 2 | Diff-stat breakdown in the stack comment | `internal/actions/pr.go`, `internal/actions/pr_test.go`, `internal/config/config.go`, `internal/gh/ghui/push.go`, `cmd/av/pr.go`, `go.mod` | Annotates each stack PR with its own `(+additions -deletions)` count and breaks each PR's diff into configurable categories with a proportional bar chart and a collapsed per-file detail. Counts are computed against the merge-base of the parent branch. Promotes `github.com/gobwas/glob` to a direct dependency. | Optional; product decision | 1 |
| 3 | Fork branding | `internal/actions/pr.go`, `internal/actions/pr_test.go` | Points the stack-comment "created with Aviator" link at this fork. Applied on the fork's `master` after modification 2 merges. | Never | 2 |