package refinery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workflow"
)

// refineryActor is the actor recorded on transitions the refinery writes.
const refineryActor = "refinery"

// maxCITail caps the failing-check names recorded on a CI CheckRun.
const maxCITail = 1000

// newRefineryWorkflowEnv builds the gated-workflow environment for the
// engineer's rig: the rig's gated config, its beads-backed Items and the
// Dolt evidence store. For a rig without gated.enabled it returns an env with
// only Config set and touches no database. Tests replace it.
var newRefineryWorkflowEnv = func(e *Engineer) (workflow.GateEnv, error) {
	cfg, err := workflow.LoadConfig(e.rig.Path)
	if err != nil {
		return workflow.GateEnv{}, fmt.Errorf("loading workflow config for rig %s: %w", e.rig.Name, err)
	}
	if !cfg.Enabled {
		return workflow.GateEnv{Config: cfg}, nil
	}
	townRoot := beads.FindTownRoot(e.rig.Path)
	if townRoot == "" {
		return workflow.GateEnv{}, fmt.Errorf("rig %s: no town root above %s", e.rig.Name, e.rig.Path)
	}
	beadsDir := beads.ResolveBeadsDir(e.rig.Path)
	dbName := beads.DatabaseNameFromMetadata(beadsDir)
	if dbName == "" {
		return workflow.GateEnv{}, fmt.Errorf("rig %s has no dolt_database in %s/metadata.json", e.rig.Name, beadsDir)
	}
	dc := doltserver.DefaultConfig(townRoot)
	user := dc.User
	if user == "" {
		user = "root"
	}
	if dc.Password != "" {
		user += ":" + dc.Password
	}
	dsn := fmt.Sprintf("%s@tcp(%s)/%s?parseTime=true&timeout=5s", user, dc.HostPort(), dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return workflow.GateEnv{}, fmt.Errorf("opening rig database %s: %w", dbName, err)
	}
	store, err := workflow.NewDoltStore(db)
	if err != nil {
		_ = db.Close()
		return workflow.GateEnv{}, err
	}
	return workflow.GateEnv{
		TownRoot: townRoot,
		Rig:      e.rig.Name,
		Store:    store,
		Items:    workflow.NewBeadsItems(beads.New(e.rig.Path)),
		Config:   cfg,
	}, nil
}

// workflowEnv returns the engineer's cached workflow env, building it on
// first use. Errors are not cached.
func (e *Engineer) workflowEnv() (workflow.GateEnv, error) {
	if e.wfEnv != nil {
		return *e.wfEnv, nil
	}
	env, err := newRefineryWorkflowEnv(e)
	if err != nil {
		return workflow.GateEnv{}, err
	}
	e.wfEnv = &env
	return env, nil
}

// workflowGateResult is the outcome of the gated merge pre-check.
type workflowGateResult struct {
	OK      bool          // merge may proceed
	Gated   bool          // the MR's source issue is a gated workflow item
	Pending bool          // CI still running; requeued with no state change
	Reason  string        // why the merge may not proceed
	Result  ProcessResult // what doMerge returns when !OK
	Head    string        // PR head to merge (the bump commit) when Gated && OK

	env    workflow.GateEnv
	itemID string
}

func gateRefuse(reason string) workflowGateResult {
	return workflowGateResult{Gated: true, Reason: reason, Result: ProcessResult{Error: reason}}
}

// gatedItem loads the MR's source issue as a gated workflow item. ok is false
// (pass-through) for a rig without gated.enabled, an MR without a source
// issue, or an issue that is not a workflow item. Any other error is returned
// so callers fail closed.
func (e *Engineer) gatedItem(mr *MRInfo) (env workflow.GateEnv, it *workflow.Item, ok bool, err error) {
	if mr == nil || strings.TrimSpace(mr.SourceIssue) == "" {
		return env, nil, false, nil
	}
	env, err = e.workflowEnv()
	if err != nil {
		return env, nil, false, fmt.Errorf("gated workflow env: %w", err)
	}
	if env.Config == nil || !env.Config.Enabled {
		return env, nil, false, nil
	}
	it, err = env.Items.Get(strings.TrimSpace(mr.SourceIssue))
	if errors.Is(err, workflow.ErrItemNotFound) {
		return env, nil, false, nil
	}
	if err != nil {
		return env, nil, false, fmt.Errorf("gated item %s: %w", mr.SourceIssue, err)
	}
	return env, it, true, nil
}

// workflowPreMerge runs the gated merge pre-check for mr. Non-gated MRs pass
// through untouched (ok=true, reason ""). See workflowGate.
func (e *Engineer) workflowPreMerge(ctx context.Context, mr *MRInfo) (ok bool, reason string) {
	r := e.workflowGate(ctx, mr)
	return r.OK, r.Reason
}

// workflowGate requires, for a gated item, in order: the item is at step
// merge; the latest human decision on review.md approves its current sha;
// review.md sets bump; the bump commit ("chore: bump version to <v>") is on
// the remote MR branch; the PR exists; and CI on the PR head passed. CI
// pending requeues with no state change. CI failure sends the item back to
// execute with a failed CheckRun and closes the MR as rejected.
func (e *Engineer) workflowGate(ctx context.Context, mr *MRInfo) workflowGateResult {
	_ = ctx
	env, it, gated, err := e.gatedItem(mr)
	if err != nil {
		reason := fmt.Sprintf("gated workflow check failed (fail closed): %v", err)
		return workflowGateResult{Reason: reason, Result: ProcessResult{Error: reason}}
	}
	if !gated {
		return workflowGateResult{OK: true}
	}
	if e.config.MergeStrategy != "pr" || e.prProvider == nil {
		return gateRefuse(fmt.Sprintf("gated item %s requires merge_strategy=pr with a PR provider", it.ID))
	}

	// 1. The item is at step merge (store-derived).
	step, history, err := workflow.CurrentStep(env, it)
	if err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: reading transitions: %v", it.ID, err))
	}
	if !history || step != "merge" {
		return gateRefuse(fmt.Sprintf("gated item %s is at step %s, not merge", it.ID, step))
	}

	// 2. The latest human decision on review.md approves its current sha.
	_, sha, fm, err := workflow.ReadArtifact(workflow.ArtifactPath(env.TownRoot, env.Rig, it.ID, "review"))
	if err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: review.md: %v", it.ID, err))
	}
	approvals, err := env.Store.Approvals(it.ID)
	if err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: reading approvals: %v", it.ID, err))
	}
	if !workflow.HumanApprovedCurrent("review", sha, approvals) {
		return gateRefuse(fmt.Sprintf("gated item %s: review.md is not human-approved at its current content", it.ID))
	}

	// 3. The approved bump.
	bump, err := workflow.ReviewBump(fm)
	if err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: %v", it.ID, err))
	}

	// 4. The bump commit on the MR branch, pushed.
	head, err := e.ensureBumpCommit(mr, env.Config.VersionFiles, bump)
	if err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: version bump: %v", it.ID, err))
	}

	// 5. The PR exists for the bumped head.
	pr, err := e.prProvider.FindPullRequest(mr.Branch, mr.PRURL, mr.PRNumber, head)
	if err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: finding PR for %s: %v", it.ID, mr.Branch, err))
	}
	if pr == nil {
		return gateRefuse(fmt.Sprintf("gated item %s: no open PR for branch %s at %s", it.ID, mr.Branch, shortSHA(head)))
	}
	if err := requirePullRequestHead(pr, head); err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: %v", it.ID, err))
	}

	// 6. CI on the PR head.
	passed, pending, err := e.prProvider.ChecksPassed(pr)
	var failed *ChecksFailedError
	switch {
	case errors.As(err, &failed):
		return e.gatedCIFailed(env, it.ID, mr, pr, failed)
	case err != nil:
		return gateRefuse(fmt.Sprintf("gated item %s: reading CI for PR #%d: %v", it.ID, pr.Number, err))
	case pending || !passed:
		reason := fmt.Sprintf("gated item %s: CI pending on PR #%d (%s)", it.ID, pr.Number, shortSHA(head))
		_, _ = fmt.Fprintf(e.output, "[Engineer] %s — requeued\n", reason)
		return workflowGateResult{Gated: true, Pending: true, Reason: reason,
			Result: ProcessResult{NeedsApproval: true, Error: reason}}
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Gated item %s: review approved, bump %s, CI passed on PR #%d\n", it.ID, bump, pr.Number)
	return workflowGateResult{OK: true, Gated: true, Head: head, env: env, itemID: it.ID}
}

// gatedCIFailed sends the item back to execute, records the failed CI run
// and closes the MR as rejected. The item bead stays open.
func (e *Engineer) gatedCIFailed(env workflow.GateEnv, id string, mr *MRInfo, pr *git.PullRequestInfo, failed *ChecksFailedError) workflowGateResult {
	reason := fmt.Sprintf("CI failed on PR #%d: %s", pr.Number, strings.Join(failed.Names, ", "))
	if err := env.Store.Transition(id, "merge", "execute", refineryActor); err != nil {
		return gateRefuse(fmt.Sprintf("gated item %s: %s; recording merge->execute: %v", id, reason, err))
	}
	if err := env.Items.SetStep(id, "execute", time.Now()); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: gated item %s: transition recorded but set step failed: %v\n", id, err)
	}
	tail := strings.Join(failed.Names, "\n")
	if len(tail) > maxCITail {
		tail = tail[:maxCITail]
	}
	if err := env.Store.AddCheck(workflow.CheckRun{ItemID: id, CmdSetSHA: "ci", Passed: false, Tail: tail, Source: "gtb", Provenance: "verified"}); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: gated item %s: recording CI failure: %v\n", id, err)
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Gated item %s: %s — back to execute\n", id, reason)
	res := e.rejectMRBeforeMerge(mr, reason)
	return workflowGateResult{Gated: true, Reason: reason, Result: res}
}

// recordGatedMerged records the gated item's terminal transition after its
// PR merged. Failures are logged: the merge itself already happened.
func (e *Engineer) recordGatedMerged(g workflowGateResult) {
	if !g.Gated || g.itemID == "" {
		return
	}
	if err := g.env.Store.Transition(g.itemID, "merge", workflow.StepDone, refineryActor); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: gated item %s merged but recording merge->done failed: %v\n", g.itemID, err)
		return
	}
	if err := g.env.Items.SetStep(g.itemID, workflow.StepDone, time.Now()); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: gated item %s: done recorded but set step failed: %v\n", g.itemID, err)
	}
}

// bumpMessage is the commit message of the version bump commit.
func bumpMessage(v string) string { return "chore: bump version to " + v }

// ensureBumpCommit makes sure the MR branch carries the version bump on top
// of the submitted head, pushes it, and adopts it as the MR's head (local
// branch ref, mr.CommitSHA and the MR bead's commit_sha), returning it.
//
// The target version is bump applied to the version on origin/<target>. The
// branch must hold that same version (it is current with the target) or
// already the target version, in which case nothing is committed. The bump
// commit is built in a temporary detached worktree so the refinery's own
// checkout is not disturbed.
//
// It is idempotent: a requeued MR whose head is already at the target
// version is not bumped again, and a remote tip that is already the bump
// commit on top of the submitted head (a run interrupted after the push) is
// adopted instead of bumped twice.
func (e *Engineer) ensureBumpCommit(mr *MRInfo, files []string, bump string) (string, error) {
	if len(files) == 0 {
		return "", workflow.ErrNoVersionFiles
	}
	submitted := strings.TrimSpace(mr.CommitSHA)
	if submitted == "" {
		return "", errors.New("missing submitted commit_sha")
	}
	base, err := e.versionAt("origin/"+mr.Target, files)
	if err != nil {
		return "", fmt.Errorf("version on origin/%s: %w", mr.Target, err)
	}
	want, err := workflow.NextVersion(base, bump)
	if err != nil {
		return "", err
	}

	tip, err := e.git.PushRemoteBranchTip("origin", mr.Branch)
	if err != nil {
		return "", fmt.Errorf("reading origin/%s: %w", mr.Branch, err)
	}
	if tip != "" && tip != submitted {
		if e.isBumpCommit(tip, submitted, want, files) {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Bump commit %s for %s already on origin/%s\n", shortSHA(tip), want, mr.Branch)
			return tip, e.adoptHead(mr, submitted, tip)
		}
		return "", fmt.Errorf("origin/%s moved from submitted head %s to %s", mr.Branch, shortSHA(submitted), shortSHA(tip))
	}

	have, err := e.versionAt(submitted, files)
	if err != nil {
		return "", fmt.Errorf("version on %s: %w", mr.Branch, err)
	}
	switch have {
	case want:
		// Already at the target version: nothing to commit.
		if tip == "" {
			if err := e.git.Push("origin", submitted+":refs/heads/"+mr.Branch, false); err != nil {
				return "", fmt.Errorf("pushing %s: %w", mr.Branch, err)
			}
		}
		return submitted, nil
	case base:
	default:
		return "", fmt.Errorf("branch %s is at version %s but origin/%s is at %s; rebase onto %s first", mr.Branch, have, mr.Target, base, mr.Target)
	}

	head, err := e.commitBump(submitted, files, bump, want)
	if err != nil {
		return "", err
	}
	if err := e.git.Push("origin", head+":refs/heads/"+mr.Branch, false); err != nil {
		return "", fmt.Errorf("pushing bump commit to %s: %w", mr.Branch, err)
	}
	if err := e.git.VerifyPushedCommit("origin", mr.Branch, head); err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Pushed %s (%s) to origin/%s\n", bumpMessage(want), shortSHA(head), mr.Branch)
	return head, e.adoptHead(mr, submitted, head)
}

// adoptHead moves the MR from its submitted head to head (a fast-forward by
// the bump commit): the local branch ref (compare-and-swap against the old
// head), the MR bead's commit_sha and mr.CommitSHA, so the existing PR
// head checks, close snapshot and post-merge proof all use the bumped head.
func (e *Engineer) adoptHead(mr *MRInfo, old, head string) error {
	if _, err := e.git.Rev("refs/heads/" + mr.Branch); err == nil {
		cmd := exec.Command("git", "update-ref", "refs/heads/"+mr.Branch, head, old)
		cmd.Dir = e.git.WorkDir()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("moving %s to bump commit: %s: %w", mr.Branch, strings.TrimSpace(string(out)), err)
		}
	}
	if id := strings.TrimSpace(mr.ID); id != "" && !e.isSyntheticMergeMechanicsMR(mr) {
		issue, err := e.beads.Show(id)
		if err != nil {
			return fmt.Errorf("reading MR %s: %w", id, err)
		}
		fields := beads.ParseMRFields(issue)
		if fields == nil {
			return fmt.Errorf("MR %s has no merge-request fields", id)
		}
		if cur := strings.TrimSpace(fields.CommitSHA); cur != old && cur != head {
			return fmt.Errorf("MR %s commit_sha changed from %s to %s", id, shortSHA(old), shortSHA(cur))
		}
		fields.CommitSHA = head
		desc := beads.SetMRFields(issue, fields)
		if err := e.beads.Update(id, beads.UpdateOptions{Description: &desc}); err != nil {
			return fmt.Errorf("recording bump head on MR %s: %w", id, err)
		}
	}
	mr.CommitSHA = head
	return nil
}

// versionAt reads the shared version of files at ref.
func (e *Engineer) versionAt(ref string, files []string) (string, error) {
	var v string
	for _, f := range files {
		content, err := e.git.ShowFile(ref, filepath.ToSlash(filepath.Clean(f)))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", f, err)
		}
		fv, err := workflow.VersionOf(f, []byte(content))
		if err != nil {
			return "", err
		}
		if v != "" && fv != v {
			return "", fmt.Errorf("version files disagree at %s: %s=%s, others %s", ref, f, fv, v)
		}
		v = fv
	}
	return v, nil
}

// isBumpCommit reports whether commit is the bump commit for version want
// directly on top of parent, with every version file at want.
func (e *Engineer) isBumpCommit(commit, parent, want string, files []string) bool {
	p, err := e.git.Rev(commit + "^")
	if err != nil || strings.TrimSpace(p) != parent {
		return false
	}
	msg, err := e.git.GetBranchCommitMessage(commit)
	if err != nil || strings.TrimSpace(msg) != bumpMessage(want) {
		return false
	}
	v, err := e.versionAt(commit, files)
	return err == nil && v == want
}

// commitBump creates the bump commit on top of parent in a temporary
// detached worktree, with the refinery's git identity, and returns its sha.
func (e *Engineer) commitBump(parent string, files []string, bump, want string) (string, error) {
	tmp, err := os.MkdirTemp("", "gtb-bump-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	wt := filepath.Join(tmp, "wt")
	if err := e.git.WorktreeAddDetached(wt, parent); err != nil {
		return "", fmt.Errorf("creating bump worktree: %w", err)
	}
	defer func() {
		if err := e.git.WorktreeRemove(wt, true); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: removing bump worktree %s: %v\n", wt, err)
		}
	}()

	got, err := workflow.ApplyBump(wt, files, bump)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("bumped to %s, want %s", got, want)
	}
	wg := git.NewGit(wt)
	if err := wg.Add(append([]string{"--"}, files...)...); err != nil {
		return "", fmt.Errorf("staging version files: %w", err)
	}
	name, _ := e.git.ConfigGet("user.name")
	email, _ := e.git.ConfigGet("user.email")
	cmd := exec.Command("git", "commit", "-m", bumpMessage(want))
	cmd.Dir = wt
	cmd.Env = os.Environ()
	if name != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
	}
	if email != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("committing bump: %s: %w", strings.TrimSpace(string(out)), err)
	}
	head, err := wg.Rev("HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(head), nil
}

// mrIsGated reports whether mr is (or may be: lookup errors count) a gated
// item. Batch processing uses it to route gated MRs through the single-MR
// path, where the gate runs.
func (e *Engineer) mrIsGated(mr *MRInfo) bool {
	_, _, gated, err := e.gatedItem(mr)
	return gated || err != nil
}
