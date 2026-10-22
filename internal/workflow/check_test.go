package workflow

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCheck_UnapprovedRefused(t *testing.T) {
	env, dir := tddEnv(t)
	env.Config.CheckCommands = []string{"go test ./...", "rm -rf /"}
	r := &fakeRunner{}
	_, err := Check(env, r, "it-1", dir)
	if err == nil || !strings.Contains(err.Error(), "check commands not approved") {
		t.Fatalf("err = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("unapproved commands must not run")
	}
	if cs, _ := env.Store.Checks("it-1"); len(cs) != 0 {
		t.Fatalf("stored = %+v", cs)
	}
}

func approvedChecks(env GateEnv, cmds ...string) {
	env.Config.CheckCommands = cmds
	env.Config.CheckCommandsApprovedSHA = CheckCommandsSHA(cmds)
}

func TestCheck_AllPass(t *testing.T) {
	env, dir := tddEnv(t)
	approvedChecks(env, "go vet ./...", "go test ./...")
	r := &fakeRunner{results: []fakeResult{{exit: 0, tail: "vet ok"}, {exit: 0, tail: "test ok"}}}
	c, err := Check(env, r, "it-1", dir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !c.Passed || c.Tail != "test ok" || c.Source != "gtb" || c.Provenance != "verified" ||
		c.CmdSetSHA != CheckCommandsSHA(env.Config.CheckCommands) || !c.At.Equal(fixedNow) || c.At.Location() != time.UTC {
		t.Fatalf("check = %+v", c)
	}
	want := []fakeCall{{dir, []string{"sh", "-c", "go vet ./..."}}, {dir, []string{"sh", "-c", "go test ./..."}}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %+v", r.calls)
	}
	if cs, _ := env.Store.Checks("it-1"); len(cs) != 1 || !cs[0].Passed {
		t.Fatalf("stored = %+v", cs)
	}
}

func TestCheck_StopsAtFirstFailure(t *testing.T) {
	env, dir := tddEnv(t)
	approvedChecks(env, "lint", "go test ./...", "never")
	r := &fakeRunner{results: []fakeResult{{exit: 0, tail: "ok"}, {exit: 1, tail: "FAIL x"}}}
	c, err := Check(env, r, "it-1", dir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if c.Passed || !strings.HasPrefix(c.Tail, "go test ./...") || !strings.HasSuffix(c.Tail, "FAIL x") {
		t.Fatalf("check = %+v", c)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %d, want stop after failure", len(r.calls))
	}
	if cs, _ := env.Store.Checks("it-1"); len(cs) != 1 || cs[0].Passed {
		t.Fatalf("stored = %+v", cs)
	}
}
