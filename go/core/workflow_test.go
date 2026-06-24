package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func appendNode(name string) NodeFn[[]string] {
	return func(_ context.Context, state []string) ([]string, error) {
		return append(append([]string{}, state...), name), nil
	}
}

func TestWorkflowLinearRunsInOrder(t *testing.T) {
	wf := NewWorkflow[[]string](0).
		AddNode("a", appendNode("a")).
		AddNode("b", appendNode("b")).
		AddNode("c", appendNode("c")).
		AddEdge("a", "b").
		AddEdge("b", "c").
		SetEntry("a").
		SetEnd("c")

	out, err := wf.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(out, ",") != "a,b,c" {
		t.Fatalf("expected a,b,c got %v", out)
	}
}

func TestWorkflowConditionalEdgeBothBranches(t *testing.T) {
	type state struct {
		n      int
		branch int
	}
	build := func() *Workflow[state] {
		return NewWorkflow[state](0).
			AddNode("start", func(_ context.Context, s state) (state, error) { return s, nil }).
			AddNode("left", func(_ context.Context, s state) (state, error) { s.branch = -1; return s, nil }).
			AddNode("right", func(_ context.Context, s state) (state, error) { s.branch = 1; return s, nil }).
			AddConditionalEdge("start", func(s state) string {
				if s.n > 0 {
					return "right"
				}
				return "left"
			}).
			SetEntry("start").
			SetEnd("left").
			SetEnd("right")
	}

	pos, err := build().Run(context.Background(), state{n: 5})
	if err != nil || pos.branch != 1 {
		t.Fatalf("positive n should route right (branch=1), got branch=%d err=%v", pos.branch, err)
	}
	neg, err := build().Run(context.Background(), state{n: -5})
	if err != nil || neg.branch != -1 {
		t.Fatalf("negative n should route left (branch=-1), got branch=%d err=%v", neg.branch, err)
	}
}

func TestWorkflowRouterEndSentinel(t *testing.T) {
	wf := NewWorkflow[int](0).
		AddNode("only", func(_ context.Context, s int) (int, error) { return s + 1, nil }).
		AddConditionalEdge("only", func(int) string { return END }).
		SetEntry("only")

	out, err := wf.Run(context.Background(), 0)
	if err != nil || out != 1 {
		t.Fatalf("expected 1, got %d err=%v", out, err)
	}
}

func TestWorkflowImplicitEndOnNoEdge(t *testing.T) {
	wf := NewWorkflow[int](0).
		AddNode("only", func(_ context.Context, s int) (int, error) { return s + 1, nil }).
		SetEntry("only")

	out, err := wf.Run(context.Background(), 0)
	if err != nil || out != 1 {
		t.Fatalf("expected 1, got %d err=%v", out, err)
	}
}

func TestWorkflowMaxStepsCapTriggersOnCycle(t *testing.T) {
	wf := NewWorkflow[[]string](6).
		AddNode("a", appendNode("a")).
		AddNode("b", appendNode("b")).
		AddEdge("a", "b").
		AddEdge("b", "a").
		SetEntry("a")

	_, err := wf.Run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "maxSteps") {
		t.Fatalf("expected maxSteps error, got %v", err)
	}
}

func TestWorkflowMissingEntryErrors(t *testing.T) {
	_, err := NewWorkflow[int](0).Run(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "no entry node") {
		t.Fatalf("expected no-entry error, got %v", err)
	}
}

func TestWorkflowUnknownEntryErrors(t *testing.T) {
	_, err := NewWorkflow[int](0).SetEntry("ghost").Run(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestWorkflowEdgeToMissingNodeErrors(t *testing.T) {
	wf := NewWorkflow[int](0).
		AddNode("a", func(_ context.Context, s int) (int, error) { return s, nil }).
		AddEdge("a", "ghost").
		SetEntry("a")

	_, err := wf.Run(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestWorkflowNodeErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	wf := NewWorkflow[int](0).
		AddNode("fail", func(_ context.Context, _ int) (int, error) { return 0, sentinel }).
		SetEntry("fail").
		SetEnd("fail")

	_, err := wf.Run(context.Background(), 0)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel error, got %v", err)
	}
}
