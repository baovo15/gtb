package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workflow"
)

// setupGatedDoneTown builds a polecat worktree for gastown/refuge with the
// gated workflow enabled for gastown, a fake bd that records every call,
// and the in-memory workflow env injected through newWorkflowEnv.
func setupGatedDoneTown(t *testing.T) (env workflow.GateEnv, bdLog string) {
	t.Helper()
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	setupRoutedSubmitGitRepo(t, workDir, false)
	bdLog = installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	townRoot := routedSourceTestTownRoot(workDir)

	settings := config.NewRigSettings()
	settings.Workflow = &config.WorkflowConfig{Gated: &config.GatedWorkflow{Enabled: true}}
	if err := config.SaveRigSettings(config.RigSettingsPath(filepath.Join(townRoot, "gastown")), settings); err != nil {
		t.Fatal(err)
	}
	env = workflow.GateEnv{
		TownRoot: townRoot,
		Rig:      "gastown",
		Store:    workflow.NewMemStore(),
		Items:    workflow.NewMemItems(),
		Config:   &config.GatedWorkflow{Enabled: true},
		Now:      func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) },
	}
	oldEnv := newWorkflowEnv
	newWorkflowEnv = func(_ *cobra.Command, rig string) (workflow.GateEnv, error) {
		if rig != "gastown" {
			t.Errorf("workflow env for rig %q, want gastown", rig)
		}
		return env, nil
	}
	t.Cleanup(func() { newWorkflowEnv = oldEnv })

	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Chdir(workDir)
	doneCleanupStatus = "unpushed"
	doneSkipVerify = true
	return env, bdLog
}

// TestDone_GatedNonMergeStepNoMR: gt done on a gated item before merge
// creates no MR bead, records the gate's human wait, and still hands off.
func TestDone_GatedNonMergeStepNoMR(t *testing.T) {
	env, bdLog := setupGatedDoneTown(t)
	it, err := env.Items.Create("gastown", workflow.KindTask, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	spec := workflow.ArtifactPath(env.TownRoot, env.Rig, it.ID, "spec")
	if err := os.MkdirAll(filepath.Dir(spec), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec, []byte("---\nstatus: draft\n---\n# spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var handedOff []string
	updateAgentStateOnDoneFn = func(_, _, exitType, issueID string) error {
		handedOff = append(handedOff, exitType+" "+issueID)
		return nil
	}
	doneIssue = it.ID
	doneStatus = ExitCompleted // the exit that would otherwise submit an MR

	if err := runDone(nil, nil); err != nil {
		t.Fatalf("runDone: %v", err)
	}
	log := readSubmitSourceBDLog(t, bdLog)
	if strings.Contains(log, "gt:merge-request") || strings.Contains(log, "Merge: "+it.ID) {
		t.Fatalf("gated non-merge step created an MR bead:\n%s", log)
	}
	got, _ := env.Items.Get(it.ID)
	if got.WaitingForHuman != "approve spec" {
		t.Errorf("waiting = %q, want %q", got.WaitingForHuman, "approve spec")
	}
	if len(handedOff) != 1 || handedOff[0] != "COMPLETED "+it.ID {
		t.Errorf("agent state handoff = %v", handedOff)
	}
}

// TestDone_GatedMergeStepUsesMRPath: at merge, gt done takes the existing MR
// submit path (it validates and resolves the source issue).
func TestDone_GatedMergeStepUsesMRPath(t *testing.T) {
	env, _ := setupGatedDoneTown(t)
	it, err := env.Items.Create("gastown", workflow.KindTask, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	steps := workflow.Steps(workflow.KindTask)
	for i := 0; i+1 < len(steps); i++ {
		if err := env.Store.Transition(it.ID, steps[i], steps[i+1], "t"); err != nil {
			t.Fatal(err)
		}
	}
	updateAgentStateOnDoneFn = func(_, _, _, _ string) error { return nil }
	doneIssue = it.ID
	err = runDone(nil, nil)
	// mem-1 is not a real bead: the MR path's source validation refuses it,
	// which proves the gated hold did not short-circuit the merge step.
	if err == nil || !strings.Contains(err.Error(), "source issue validation failed") {
		t.Fatalf("runDone at merge = %v, want the MR path's source validation", err)
	}
	if got, _ := env.Items.Get(it.ID); got.WaitingForHuman != "" {
		t.Errorf("merge step recorded a wait: %q", got.WaitingForHuman)
	}
}
