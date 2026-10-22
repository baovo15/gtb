package workflow

import (
	"errors"
	"testing"
)

func TestMemStore_TransitionConflict(t *testing.T) {
	s := NewMemStore()
	if err := s.Transition("it-1", "spec", "plan", "tester"); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if err := s.Transition("it-1", "spec", "plan", "tester"); !errors.Is(err, ErrStepConflict) {
		t.Fatalf("second transition err = %v, want ErrStepConflict", err)
	}
	trs, err := s.Transitions("it-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 1 || trs[0].From != "spec" || trs[0].To != "plan" || trs[0].Actor != "tester" {
		t.Fatalf("transitions = %+v", trs)
	}
	if err := s.Transition("it-1", "plan", "execute", "tester"); err != nil {
		t.Fatalf("chained transition: %v", err)
	}
}
