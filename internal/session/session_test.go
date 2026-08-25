package session

import "testing"

func TestSaveLoad(t *testing.T) {
	ws := t.TempDir()
	s := New("run-1", ws, "do a thing")
	s.AddTool(ToolTrace{Name: "exec", Result: "ok"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "do a thing" || len(got.ToolLog) != 1 {
		t.Fatalf("%+v", got)
	}
}
