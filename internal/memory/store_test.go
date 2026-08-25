package memory

import (
	"testing"
)

func TestSessionRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ScopeSession, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Read(ScopeSession, "k")
	if err != nil || !ok || got != "v1" {
		t.Fatalf("got %q ok=%v err=%v", got, ok, err)
	}
}

func TestLongTermAppendOnlyLastWriteWins(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ScopeLongTerm, "fact", "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ScopeLongTerm, "fact", "two"); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ScopeLongTerm, "other", "x"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Read(ScopeLongTerm, "fact")
	if err != nil || !ok || got != "two" {
		t.Fatalf("got %q ok=%v err=%v", got, ok, err)
	}
}

func TestUnknownScope(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := s.Write("cloud", "k", "v"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMissingKey(t *testing.T) {
	s, _ := Open(t.TempDir())
	_, ok, err := s.Read(ScopeSession, "nope")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}
