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

// twoNodeChild is a child graph of two tracking nodes joined by a conditional
// edge that skips a third — proving the whole sub-graph, routing included, runs
// inside the parent's single step.
func twoNodeChild() *Workflow[[]string] {
	return NewWorkflow[[]string](0).
		AddNode("child_a", appendNode("child_a")).
		AddNode("child_b", appendNode("child_b")).
		AddNode("child_never", appendNode("child_never")).
		AddConditionalEdge("child_a", func(state []string) string {
			for _, s := range state {
				if s == "child_a" {
					return "child_b"
				}
			}
			return "child_never"
		}).
		SetEntry("child_a").
		SetEnd("child_b")
}

func identity(state []string) []string { return state }

func takeChild(_ []string, child []string) []string { return child }

func TestSubWorkflowRunsToCompletionInOneStep(t *testing.T) {
	wf := NewWorkflow[[]string](0).
		AddNode("parent_a", appendNode("parent_a")).
		AddNode("sub", SubWorkflowNode(twoNodeChild(), identity, takeChild)).
		AddNode("parent_b", appendNode("parent_b")).
		AddEdge("parent_a", "sub").
		AddEdge("sub", "parent_b").
		SetEntry("parent_a").
		SetEnd("parent_b")

	out, err := wf.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(out, ","); got != "parent_a,child_a,child_b,parent_b" {
		t.Fatalf("expected the sub-workflow to run fully in one step, got %q", got)
	}
}

func TestSubWorkflowStateMapsInAndOut(t *testing.T) {
	// Parent state is a labelled total; the child only ever sees the int.
	type parent struct {
		Label string
		Total int
	}

	child := NewWorkflow[int](0).
		AddNode("add_ten", func(_ context.Context, n int) (int, error) { return n + 10, nil }).
		AddNode("double", func(_ context.Context, n int) (int, error) { return n * 2, nil }).
		AddEdge("add_ten", "double").
		SetEntry("add_ten").
		SetEnd("double")

	wf := NewWorkflow[parent](0).
		AddNode("math", SubWorkflowNode(child,
			func(p parent) int { return p.Total },
			func(p parent, total int) parent { return parent{Label: p.Label + ":done", Total: total} },
		)).
		SetEntry("math").
		SetEnd("math")

	out, err := wf.Run(context.Background(), parent{Label: "start", Total: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Label != "start:done" || out.Total != 30 {
		t.Fatalf("expected {start:done 30}, got %+v", out)
	}
}

func TestSubWorkflowErrorPropagatesToParent(t *testing.T) {
	sentinel := errors.New("child exploded")
	child := NewWorkflow[[]string](0).
		AddNode("boom", func(_ context.Context, _ []string) ([]string, error) { return nil, sentinel }).
		SetEntry("boom").
		SetEnd("boom")

	wf := NewWorkflow[[]string](0).
		AddNode("parent_a", appendNode("parent_a")).
		AddNode("sub", SubWorkflowNode(child, identity, takeChild)).
		AddNode("parent_b", appendNode("parent_b")).
		AddEdge("parent_a", "sub").
		AddEdge("sub", "parent_b").
		SetEntry("parent_a").
		SetEnd("parent_b")

	_, err := wf.Run(context.Background(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the child error to propagate, got %v", err)
	}
}

func TestSubWorkflowNestingDepthTwo(t *testing.T) {
	grandchild := NewWorkflow[[]string](0).
		AddNode("grand_a", appendNode("grand_a")).
		AddNode("grand_b", appendNode("grand_b")).
		AddEdge("grand_a", "grand_b").
		SetEntry("grand_a").
		SetEnd("grand_b")

	child := NewWorkflow[[]string](0).
		AddNode("child_a", appendNode("child_a")).
		AddNode("grand", SubWorkflowNode(grandchild, identity, takeChild)).
		AddEdge("child_a", "grand").
		SetEntry("child_a").
		SetEnd("grand")

	wf := NewWorkflow[[]string](0).
		AddNode("parent_a", appendNode("parent_a")).
		AddNode("sub", SubWorkflowNode(child, identity, takeChild)).
		AddEdge("parent_a", "sub").
		SetEntry("parent_a").
		SetEnd("sub")

	out, err := wf.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(out, ","); got != "parent_a,child_a,grand_a,grand_b" {
		t.Fatalf("expected two levels of nesting to run fully, got %q", got)
	}
}

// Plain nodes and sub-workflows are interchangeable vertices: one composite graph
// wires both with the same conditional edges — routing INTO a sub-workflow and OUT
// of one — and the sub-workflow itself mixes a plain node with a nested
// sub-workflow (depth 2). All of it in one parent run.
func TestCompositeGraphMixesPlainAndSubWorkflowVertices(t *testing.T) {
	contains := func(state []string, want string) bool {
		for _, s := range state {
			if s == want {
				return true
			}
		}
		return false
	}

	deep := NewWorkflow[[]string](0).
		AddNode("deep_a", appendNode("deep_a")).
		AddNode("deep_b", appendNode("deep_b")).
		AddNode("deep_never", appendNode("deep_never")).
		AddConditionalEdge("deep_a", func(state []string) string {
			if contains(state, "deep_a") {
				return "deep_b"
			}
			return "deep_never"
		}).
		SetEntry("deep_a").
		SetEnd("deep_b")

	// A sub-workflow whose own vertices are a plain node AND a sub-workflow.
	enrich := NewWorkflow[[]string](0).
		AddNode("enrich_a", appendNode("enrich_a")).
		AddNode("deep", SubWorkflowNode(deep, identity, takeChild)).
		AddEdge("enrich_a", "deep").
		SetEntry("enrich_a").
		SetEnd("deep")

	// Parent: plain --conditional--> sub-workflow --conditional--> plain.
	wf := NewWorkflow[[]string](0).
		AddNode("classify", appendNode("classify")).
		AddNode("enrich", SubWorkflowNode(enrich, identity, takeChild)).
		AddNode("finish", appendNode("finish")).
		AddNode("never", appendNode("never")).
		AddConditionalEdge("classify", func(state []string) string {
			if contains(state, "classify") {
				return "enrich"
			}
			return "never"
		}).
		// A conditional edge LEAVING a sub-workflow vertex, routing on state the
		// sub-workflow produced — including the terminate sentinel.
		AddConditionalEdge("enrich", func(state []string) string {
			if contains(state, "deep_b") {
				return "finish"
			}
			return END
		}).
		SetEntry("classify").
		SetEnd("finish")

	out, err := wf.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(out, ","); got != "classify,enrich_a,deep_a,deep_b,finish" {
		t.Fatalf("expected plain and sub-workflow vertices to compose in one run, got %q", got)
	}
}
