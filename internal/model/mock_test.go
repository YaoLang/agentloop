package model

import (
	"context"
	"testing"
)

func TestScriptedWalksThenStops(t *testing.T) {
	m := NewScripted([]Step{
		{Tool: "read_file", Args: map[string]any{"path": "a.txt"}},
		{Content: "done"},
	})
	r1, err := m.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Message.ToolCalls) != 1 || r1.Message.ToolCalls[0].Name != "read_file" {
		t.Fatalf("first step: %+v", r1.Message)
	}
	r2, err := m.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "tool", Content: "ok"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Message.Content != "done" {
		t.Fatalf("content = %q", r2.Message.Content)
	}
	r3, err := m.Complete(context.Background(), CompleteRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r3.Message.Content != "script complete" {
		t.Fatalf("exhausted = %q", r3.Message.Content)
	}
}

func TestHeuristicIsDeterministic(t *testing.T) {
	a := NewMock()
	b := NewMock()
	req := CompleteRequest{Messages: []Message{{Role: "user", Content: "ship it"}}}
	ra, _ := a.Complete(context.Background(), req)
	rb, _ := b.Complete(context.Background(), req)
	if ra.Message.ToolCalls[0].Name != rb.Message.ToolCalls[0].Name {
		t.Fatalf("non-deterministic heuristic: %v vs %v", ra.Message, rb.Message)
	}
	if ra.Message.ToolCalls[0].Name != "write_file" {
		t.Fatalf("expected write_file first, got %s", ra.Message.ToolCalls[0].Name)
	}
}

func TestCostUSD(t *testing.T) {
	if c := CostUSD(1_000_000, 0); c != 0.15 {
		t.Fatalf("prompt cost = %v", c)
	}
	if c := CostUSD(0, 1_000_000); c != 0.60 {
		t.Fatalf("completion cost = %v", c)
	}
}
