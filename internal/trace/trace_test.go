package trace

import (
	"path/filepath"
	"testing"
)

func TestLogAndRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.jsonl")
	w, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := w.Log(Event{Type: "model_call", Step: 1, Model: "mock", LatencyMS: 2}); err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Event{Type: "tool_call", Name: "exec", OK: &ok}); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("len=%d", len(evs))
	}
	if evs[0].Type != "model_call" || evs[1].Name != "exec" {
		t.Fatalf("%+v", evs)
	}
}
