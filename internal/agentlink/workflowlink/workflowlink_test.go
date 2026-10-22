package workflowlink

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentlink"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workflow"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type fixture struct {
	town  string
	env   workflow.GateEnv
	queue *agentlink.Queue
	app   *WorkflowApplier
	col   *WorkflowCollector

	slung, cancelled []string
	peeked           string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "r"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{town: town, queue: agentlink.OpenQueue(town)}
	f.env = workflow.GateEnv{
		TownRoot: town, Rig: "r",
		Store:  workflow.NewMemStore(),
		Items:  workflow.NewMemItems(),
		Config: &config.GatedWorkflow{Enabled: true},
	}
	f.env = Observe(f.env, f.queue, nil)
	rigs := func() ([]agentlink.Rig, error) {
		return []agentlink.Rig{{Name: "r", Remote: "git@x:r.git", Gated: true}, {Name: "plain"}}, nil
	}
	envFor := func(rig string) (workflow.GateEnv, error) {
		if rig != "r" {
			return workflow.GateEnv{}, errors.New("no env for " + rig)
		}
		return f.env, nil
	}
	f.app = &WorkflowApplier{
		TownRoot: town,
		Rigs:     rigs,
		Env:      envFor,
		Sling: func(_ context.Context, id, rig string) error {
			f.slung = append(f.slung, id+"@"+rig)
			return nil
		},
		Cancel: func(_ context.Context, id string) error {
			f.cancelled = append(f.cancelled, id)
			return nil
		},
		Peek: func(_ context.Context, rig, id string) (string, error) { return f.peeked, nil },
		Now:  func() time.Time { return t0 },
	}
	f.col = &WorkflowCollector{
		TownRoot: town,
		Rigs:     rigs,
		Env:      envFor,
		Queue:    f.queue,
		Sessions: func() ([]agentlink.SessionView, error) {
			return []agentlink.SessionView{{Name: "r-toast", Role: "polecat", Rig: "r", ItemID: "mem-1"}}, nil
		},
		DiffStat: func(rig, branch string) ([]agentlink.FileStat, error) {
			return []agentlink.FileStat{{Path: "a.go", Added: 3, Deleted: 1}}, nil
		},
	}
	return f
}

// item creates a task with its spec artifact and returns its ID and the
// artifact's sha.
func (f *fixture) item(t *testing.T, content string) (string, string) {
	t.Helper()
	it, err := f.env.Items.Create("r", workflow.KindTask, "Title", "")
	if err != nil {
		t.Fatal(err)
	}
	p := workflow.ArtifactPath(f.town, "r", it.ID, "spec")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sha, _, err := workflow.ReadArtifact(p)
	if err != nil {
		t.Fatal(err)
	}
	return it.ID, sha
}

func req(typ, item string, params any) agentlink.Request {
	b, _ := json.Marshal(params)
	return agentlink.Request{Key: "k-" + typ, Type: typ, ItemID: item, Params: b, ExpiresAt: t0.Add(time.Hour)}
}

func approveReq(item, sha string) agentlink.Request {
	return req("approve", item, agentlink.ApproveParams{Artifact: "spec", SHA: sha, Actor: "baovo15"})
}

func TestApplyApprove_StaleSHA(t *testing.T) {
	f := newFixture(t)
	id, sha := f.item(t, "---\nstatus: draft\n---\nv1\n")
	// The artifact changes after the dashboard read it.
	p := workflow.ArtifactPath(f.town, "r", id, "spec")
	if err := os.WriteFile(p, []byte("---\nstatus: draft\n---\nv2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.app.Apply(context.Background(), approveReq(id, sha))
	if res.OK || res.Error != "stale_sha" || res.Key != "k-approve" {
		t.Fatalf("result = %+v, want stale_sha", res)
	}
	if as, _ := f.env.Store.Approvals(id); len(as) != 0 {
		t.Fatalf("approvals recorded on stale sha: %+v", as)
	}
}

func TestApplyApprove_RecordsDashboardChannel(t *testing.T) {
	f := newFixture(t)
	id, sha := f.item(t, "spec body\n")
	for _, typ := range []string{"approve", "request_changes"} {
		r := approveReq(id, sha)
		r.Type, r.Key = typ, "k-"+typ
		if res := f.app.Apply(context.Background(), r); !res.OK {
			t.Fatalf("%s: %+v", typ, res)
		}
	}
	as, _ := f.env.Store.Approvals(id)
	if len(as) != 2 {
		t.Fatalf("approvals = %+v", as)
	}
	for i, verdict := range []string{"approve", "request_changes"} {
		a := as[i]
		want := workflow.Approval{ItemID: id, Artifact: "spec", SHA: sha, Verdict: verdict, Role: "human", Source: "human", Channel: "dashboard", Actor: "baovo15", At: t0}
		if a != want {
			t.Fatalf("approval[%d] = %+v, want %+v", i, a, want)
		}
	}
	if !workflow.HumanApprovedCurrent("spec", sha, as[:1]) {
		t.Fatal("dashboard approval not accepted by the gate")
	}
	// Each approval is queued as an evidence event.
	pend, _ := f.queue.Pending(100)
	n := 0
	for _, e := range pend {
		if e.Type == agentlink.EventEvidence && e.ItemID == id && strings.Contains(string(e.Data), `"channel":"dashboard"`) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("evidence events = %d in %+v", n, pend)
	}
}

func TestApplyApprove_Validation(t *testing.T) {
	f := newFixture(t)
	id, sha := f.item(t, "x\n")
	cases := map[string]agentlink.Request{
		"bad id":       approveReq("../etc", sha),
		"bad artifact": req("approve", id, agentlink.ApproveParams{Artifact: "../spec", SHA: sha, Actor: "a"}),
		"no actor":     req("approve", id, agentlink.ApproveParams{Artifact: "spec", SHA: sha}),
		"no sha":       req("approve", id, agentlink.ApproveParams{Artifact: "spec", Actor: "a"}),
		"bad json":     {Key: "k", Type: "approve", ItemID: id, Params: json.RawMessage(`[`), ExpiresAt: t0.Add(time.Hour)},
	}
	for name, r := range cases {
		if res := f.app.Apply(context.Background(), r); res.OK || res.Error != agentlink.ErrCodeInvalid {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if res := f.app.Apply(context.Background(), approveReq("gt-missing", sha)); res.OK || res.Error != agentlink.ErrCodeNotFound {
		t.Errorf("unknown item: %+v", res)
	}
	if as, _ := f.env.Store.Approvals(id); len(as) != 0 {
		t.Fatalf("recorded: %+v", as)
	}
}

func TestApply_UnknownTypeUnsupported(t *testing.T) {
	f := newFixture(t)
	res := f.app.Apply(context.Background(), agentlink.Request{Key: "k1", Type: "shell", ExpiresAt: t0.Add(time.Hour)})
	if res.OK || res.Error != "unsupported" || res.Key != "k1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestApply_ExpiredNoSideEffects(t *testing.T) {
	f := newFixture(t)
	id, sha := f.item(t, "x\n")
	r := approveReq(id, sha)
	r.ExpiresAt = t0.Add(-time.Second)
	if res := f.app.Apply(context.Background(), r); res.OK || res.Error != "expired" {
		t.Fatalf("result = %+v", res)
	}
	s := req("item.sling", id, nil)
	s.ExpiresAt = t0
	f.app.Apply(context.Background(), s)
	if as, _ := f.env.Store.Approvals(id); len(as) != 0 || len(f.slung) != 0 {
		t.Fatalf("side effects: %+v %v", as, f.slung)
	}
}

func TestApply_ItemCreate(t *testing.T) {
	f := newFixture(t)
	res := f.app.Apply(context.Background(), req("item.create", "", map[string]string{"rig": "r", "kind": "fix", "title": "  Broken   thing ", "parent": ""}))
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	var out struct{ ID, Rig, Kind, Step string }
	json.Unmarshal(res.Data, &out)
	if out.Rig != "r" || out.Kind != "fix" || out.Step != "report" {
		t.Fatalf("data = %s", res.Data)
	}
	it, err := f.env.Items.Get(out.ID)
	if err != nil || it.Title != "Broken thing" {
		t.Fatalf("item = %+v, %v", it, err)
	}
	b, err := os.ReadFile(workflow.ArtifactPath(f.town, "r", out.ID, "report"))
	if err != nil || !strings.Contains(string(b), "status: draft") || !strings.Contains(string(b), "# Broken thing") {
		t.Fatalf("stub = %q, %v", b, err)
	}
	pend, _ := f.queue.Pending(100)
	if len(pend) == 0 || pend[len(pend)-1].Type != agentlink.EventItemUpsert || pend[len(pend)-1].ItemID != out.ID {
		t.Fatalf("no upsert queued: %+v", pend)
	}
	for name, p := range map[string]map[string]string{
		"ungated rig": {"rig": "plain", "kind": "task", "title": "t"},
		"bad rig":     {"rig": "../r", "kind": "task", "title": "t"},
		"bad kind":    {"rig": "r", "kind": "chore", "title": "t"},
		"no title":    {"rig": "r", "kind": "task", "title": "  "},
		"bad parent":  {"rig": "r", "kind": "task", "title": "t", "parent": "a b"},
	} {
		if res := f.app.Apply(context.Background(), req("item.create", "", p)); res.OK || res.Error != agentlink.ErrCodeInvalid {
			t.Errorf("%s: %+v", name, res)
		}
	}
}

func TestApply_SlingCancelSnapshot(t *testing.T) {
	f := newFixture(t)
	id, _ := f.item(t, "x\n")
	if res := f.app.Apply(context.Background(), req("item.sling", id, nil)); !res.OK {
		t.Fatalf("sling: %+v", res)
	}
	if res := f.app.Apply(context.Background(), req("item.cancel", id, nil)); !res.OK {
		t.Fatalf("cancel: %+v", res)
	}
	if len(f.slung) != 1 || f.slung[0] != id+"@r" || len(f.cancelled) != 1 || f.cancelled[0] != id {
		t.Fatalf("slung=%v cancelled=%v", f.slung, f.cancelled)
	}
	f.app.Sling = func(context.Context, string, string) error { return errors.New("exit 1: no capacity") }
	res := f.app.Apply(context.Background(), req("item.sling", id, nil))
	if res.OK || res.Error != agentlink.ErrCodeFailed || !strings.Contains(string(res.Data), "no capacity") {
		t.Fatalf("failed sling: %+v", res)
	}
	if res := f.app.Apply(context.Background(), req("item.sling", "gt-nope", nil)); res.OK || res.Error != agentlink.ErrCodeNotFound {
		t.Fatalf("unknown item: %+v", res)
	}
	if res := f.app.Apply(context.Background(), req("snapshot", "", nil)); !res.OK {
		t.Fatalf("snapshot: %+v", res)
	}
}

func TestApply_InspectAndPeek(t *testing.T) {
	f := newFixture(t)
	id, sha := f.item(t, "spec\n")
	if err := f.env.Items.SetBranch(id, "polecat/toast/"+id); err != nil {
		t.Fatal(err)
	}
	res := f.app.Apply(context.Background(), req("inspect", id, nil))
	if !res.OK {
		t.Fatalf("inspect: %+v", res)
	}
	var iv agentlink.ItemView
	json.Unmarshal(res.Data, &iv)
	if iv.ID != id || iv.Step != "spec" || len(iv.Artifacts) != 1 || iv.Artifacts[0].SHA != sha || iv.Artifacts[0].Content != "spec\n" || len(iv.Diffstat) != 0 {
		t.Fatalf("inspect view = %+v", iv)
	}
	f.peeked = strings.Repeat("é", 40<<10) // 80 KiB
	res = f.app.Apply(context.Background(), req("terminal.peek", id, nil))
	var pk struct {
		Lines string `json:"lines"`
	}
	json.Unmarshal(res.Data, &pk)
	if !res.OK || len(pk.Lines) > 64<<10 || len(pk.Lines) < 60<<10 || !strings.HasSuffix(f.peeked, pk.Lines) {
		t.Fatalf("peek: ok=%v len=%d", res.OK, len(pk.Lines))
	}
}

func TestObserver_TransitionQueuesUpsert(t *testing.T) {
	f := newFixture(t)
	id, sha := f.item(t, "spec\n")
	f.env.Store.AddApproval(workflow.Approval{ItemID: id, Artifact: "spec", SHA: sha, Verdict: "approve", Role: "human", Source: "human", Channel: "terminal", At: time.Now()})
	before, _ := f.queue.Pending(100)
	if _, res, err := workflow.Advance(f.env, id, "me"); err != nil || !res.Passed {
		t.Fatalf("advance: %+v %v", res, err)
	}
	pend, _ := f.queue.Pending(100)
	var views []agentlink.ItemView
	for _, e := range pend[len(before):] {
		if e.Type == agentlink.EventItemUpsert && e.ItemID == id {
			var iv agentlink.ItemView
			json.Unmarshal(e.Data, &iv)
			views = append(views, iv)
		}
	}
	if len(views) == 0 || views[0].Step != "plan" || views[0].Rig != "r" || views[len(views)-1].Step != "plan" {
		t.Fatalf("upserts = %+v", views)
	}
	if err := f.env.Items.SetWaiting(id, "approve plan"); err != nil {
		t.Fatal(err)
	}
	pend, _ = f.queue.Pending(100)
	var last agentlink.ItemView
	json.Unmarshal(pend[len(pend)-1].Data, &last)
	if last.WaitingForHuman != "approve plan" {
		t.Fatalf("waiting upsert = %+v", last)
	}
}

func TestObserve_NilQueueIsNoop(t *testing.T) {
	env := workflow.GateEnv{Items: workflow.NewMemItems()}
	got := Observe(env, nil, nil)
	if got.Observer != nil || got.Items != env.Items {
		t.Fatal("Observe with nil queue changed the env")
	}
}

func TestCollector_SnapshotAndScan(t *testing.T) {
	f := newFixture(t)
	id, sha := f.item(t, "spec\n")
	f.env.Items.SetBranch(id, "polecat/toast/"+id)
	// Source files in the item dir are never sent; only <name>.md artifacts.
	dir := filepath.Dir(workflow.ArtifactPath(f.town, "r", id, "spec"))
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package x"), 0o600)
	os.MkdirAll(filepath.Join(dir, "outputs"), 0o700)
	os.WriteFile(filepath.Join(dir, "outputs", "run.md"), []byte("log"), 0o600)
	f.queue.Append(agentlink.Event{Type: agentlink.EventHeartbeat})
	pend, _ := f.queue.Pending(10)
	f.queue.Ack(pend[len(pend)-1].ID) // as if the loop had sent everything so far
	acked := pend[len(pend)-1].ID

	snap, err := f.col.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Rigs) != 2 || !snap.Rigs[0].Gated || snap.Cursor != acked {
		t.Fatalf("rigs/cursor = %+v %s", snap.Rigs, snap.Cursor)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("items = %+v", snap.Items)
	}
	iv := snap.Items[0]
	if iv.ID != id || iv.Branch == "" || len(iv.Diffstat) != 1 || len(iv.Artifacts) != 1 || iv.Artifacts[0].Name != "spec" || iv.Artifacts[0].SHA != sha {
		t.Fatalf("item view = %+v", iv)
	}
	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}

	// Nothing changed since the snapshot: the scan queues nothing.
	if err := f.col.Scan(); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.queue.Pending(10); len(p) != 0 {
		t.Fatalf("scan after snapshot queued %+v", p)
	}
	// An edit is queued once, as an artifact event with the new sha.
	p := workflow.ArtifactPath(f.town, "r", id, "spec")
	os.WriteFile(p, []byte("spec v2\n"), 0o600)
	f.col.Scan()
	f.col.Scan()
	got, _ := f.queue.Pending(10)
	if len(got) != 1 || got[0].Type != agentlink.EventArtifact || got[0].ItemID != id {
		t.Fatalf("scan events = %+v", got)
	}
	var av agentlink.ArtifactView
	json.Unmarshal(got[0].Data, &av)
	_, sha2, _, _ := workflow.ReadArtifact(p)
	if av.Name != "spec" || av.SHA != sha2 || av.Content != "spec v2\n" {
		t.Fatalf("artifact = %+v", av)
	}
	// A fresh collector (daemon restart) remembers what was sent.
	col2 := &WorkflowCollector{TownRoot: f.col.TownRoot, Rigs: f.col.Rigs, Env: f.col.Env, Queue: f.col.Queue, DiffStat: f.col.DiffStat}
	f.queue.Ack(got[0].ID)
	col2.Scan()
	if p, _ := f.queue.Pending(10); len(p) != 0 {
		t.Fatalf("restart rescan queued %+v", p)
	}
}

func TestCollector_OversizedArtifactNotSent(t *testing.T) {
	f := newFixture(t)
	id, _ := f.item(t, strings.Repeat("x", maxArtifactBytes+1))
	snap, err := f.col.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 1 || len(snap.Items[0].Artifacts) != 0 {
		t.Fatalf("oversized artifact sent: %+v", snap.Items)
	}
	_ = id
}

func TestParseNumStat(t *testing.T) {
	got := parseNumStat("3\t1\ta.go\n-\t-\tbin.png\n10\t0\tdir/b.go\n")
	want := []agentlink.FileStat{{Path: "a.go", Added: 3, Deleted: 1}, {Path: "bin.png"}, {Path: "dir/b.go", Added: 10}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
