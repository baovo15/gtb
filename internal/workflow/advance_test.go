package workflow

import (
	"errors"
	"testing"
	"time"
)

type recObserver struct {
	kinds []string
	ids   []string
	data  []any
}

func (r *recObserver) OnEvent(kind, itemID string, data any) {
	r.kinds = append(r.kinds, kind)
	r.ids = append(r.ids, itemID)
	r.data = append(r.data, data)
}

func TestAdvance_GateFailNoTransition(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindTask, "spec", time.Now())
	writeArtifact(t, env, it.ID, "spec", "body\n")
	obs := &recObserver{}
	env.Observer = obs
	got, res, err := Advance(env, it.ID, "me")
	if err != nil {
		t.Fatalf("Advance err = %v", err)
	}
	assertGate(t, res, false, "approve spec", "spec.md not approved")
	if got == nil || got.Step != "spec" {
		t.Fatalf("item = %+v, want step spec", got)
	}
	trs, _ := env.Store.Transitions(it.ID)
	if len(trs) != 0 {
		t.Fatalf("transitions = %+v, want none", trs)
	}
	if len(obs.kinds) != 0 {
		t.Fatalf("observer got %v on gate failure", obs.kinds)
	}
}

func TestAdvance_PassMovesStep(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindTask, "spec", time.Now().Add(-time.Hour))
	writeArtifact(t, env, it.ID, "spec", "body\n")
	approve(t, env, it.ID, "spec")
	obs := &recObserver{}
	env.Observer = obs
	got, res, err := Advance(env, it.ID, "me")
	if err != nil {
		t.Fatalf("Advance err = %v", err)
	}
	assertGate(t, res, true, "", "")
	if got.Step != "plan" {
		t.Fatalf("returned step = %s, want plan", got.Step)
	}
	stored, err := env.Items.Get(it.ID)
	must(t, err)
	if stored.Step != "plan" {
		t.Fatalf("Items.Get step = %s, want plan", stored.Step)
	}
	trs, _ := env.Store.Transitions(it.ID)
	if len(trs) != 1 || trs[0].From != "spec" || trs[0].To != "plan" || trs[0].Actor != "me" {
		t.Fatalf("transitions = %+v", trs)
	}
	if !stored.StepEnteredAt.Equal(trs[0].At) {
		t.Fatalf("StepEnteredAt = %v, want transition time %v", stored.StepEnteredAt, trs[0].At)
	}
	if len(obs.kinds) != 1 || obs.kinds[0] != "transition" || obs.ids[0] != it.ID {
		t.Fatalf("observer = %+v", obs)
	}
	if tr, ok := obs.data[0].(Transition); !ok || tr.From != "spec" || tr.To != "plan" || tr.Actor != "me" {
		t.Fatalf("observer data = %#v", obs.data[0])
	}
}

func TestAdvance_NilObserver(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindTask, "spec", time.Now())
	writeArtifact(t, env, it.ID, "spec", "body\n")
	approve(t, env, it.ID, "spec")
	if _, res, err := Advance(env, it.ID, "me"); err != nil || !res.Passed {
		t.Fatalf("Advance = %+v, %v", res, err)
	}
}

func TestAdvance_UsesStoreStep(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindTask, "spec", time.Now())
	// The store already moved the item to plan; the bead is stale.
	must(t, env.Store.Transition(it.ID, "spec", "plan", "other"))
	writeArtifact(t, env, it.ID, "plan", "---\nsteps: a\n---\n")
	approve(t, env, it.ID, "plan")
	got, res, err := Advance(env, it.ID, "me")
	if err != nil || !res.Passed {
		t.Fatalf("Advance = %+v, %v", res, err)
	}
	if got.Step != "execute" {
		t.Fatalf("step = %s, want execute", got.Step)
	}
	trs, _ := env.Store.Transitions(it.ID)
	if len(trs) != 2 || trs[1].From != "plan" || trs[1].To != "execute" {
		t.Fatalf("transitions = %+v", trs)
	}
}

// conflictStore races: another actor advances the item between Gate and
// Transition.
type conflictStore struct{ Store }

func (c conflictStore) Transition(id string, from, to Step, actor string) error {
	_ = c.Store.Transition(id, from, to, "racer")
	return c.Store.Transition(id, from, to, actor)
}

func TestAdvance_ConflictReturned(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindTask, "spec", time.Now())
	writeArtifact(t, env, it.ID, "spec", "body\n")
	approve(t, env, it.ID, "spec")
	env.Store = conflictStore{env.Store}
	_, _, err := Advance(env, it.ID, "me")
	if !errors.Is(err, ErrStepConflict) {
		t.Fatalf("err = %v, want ErrStepConflict", err)
	}
	trs, _ := env.Store.Transitions(it.ID)
	if len(trs) != 1 || trs[0].Actor != "racer" {
		t.Fatalf("transitions = %+v", trs)
	}
}

type failSetStep struct{ Items }

var errSetStep = errors.New("bead write failed")

func (f failSetStep) SetStep(string, Step, time.Time) error { return errSetStep }

func TestAdvance_SetStepFailureKeepsTransition(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindTask, "spec", time.Now())
	writeArtifact(t, env, it.ID, "spec", "body\n")
	approve(t, env, it.ID, "spec")
	env.Items = failSetStep{env.Items}
	obs := &recObserver{}
	env.Observer = obs
	got, res, err := Advance(env, it.ID, "me")
	if !errors.Is(err, errSetStep) {
		t.Fatalf("err = %v, want wrapped SetStep error", err)
	}
	if !res.Passed || got == nil || got.Step != "plan" {
		t.Fatalf("item = %+v, res = %+v", got, res)
	}
	trs, _ := env.Store.Transitions(it.ID)
	if len(trs) != 1 || trs[0].To != "plan" {
		t.Fatalf("transition rolled back: %+v", trs)
	}
	if len(obs.kinds) != 1 {
		t.Fatalf("observer = %v, want the transition event", obs.kinds)
	}
}

func TestAdvance_MergeNeverPasses(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindTask, "merge", time.Now())
	_, res, err := Advance(env, it.ID, "me")
	if err != nil {
		t.Fatal(err)
	}
	assertGate(t, res, false, "", "merge is performed by the refinery")
}

func TestAdvance_LastEpicStep(t *testing.T) {
	env := newTestEnv(t)
	it := newItemAt(t, env, KindEpic, "verification", time.Now())
	writeArtifact(t, env, it.ID, "verification", "ok\n")
	approve(t, env, it.ID, "verification")
	_, res, err := Advance(env, it.ID, "me")
	if err != nil {
		t.Fatal(err)
	}
	assertGate(t, res, false, "", "verification is the last step")
	trs, _ := env.Store.Transitions(it.ID)
	if len(trs) != 0 {
		t.Fatalf("transitions = %+v", trs)
	}
}

func TestAdvance_UnknownItem(t *testing.T) {
	env := newTestEnv(t)
	if _, _, err := Advance(env, "nope", "me"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}
