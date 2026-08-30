package inject

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/YaoLang/agentloop/internal/model"
)

func TestLoadCatalogAndMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")
	raw := `{
  "rules": [
    {
      "id": "hours",
      "match": {"goal_contains": ["营业时间", "opening hours"]},
      "messages": [
        {"role": "user", "content": "你们几点开门？"},
        {"role": "assistant", "content": "周一至周五 9:00-18:00。"}
      ]
    },
    {
      "id": "ping",
      "match": {"goal_prefix": "ping"},
      "direct_reply": "pong for {{goal}}"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(raw), 0o640); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	hit := cat.Match("请问营业时间？")
	if hit == nil || hit.RuleID != "hours" {
		t.Fatalf("hours hit=%+v", hit)
	}
	if len(hit.Messages) != 2 || hit.Messages[1].Content == "" {
		t.Fatalf("messages=%+v", hit.Messages)
	}
	if hit.DirectReply != "" {
		t.Fatalf("unexpected direct_reply=%q", hit.DirectReply)
	}

	hit = cat.Match("ping test")
	if hit == nil || hit.RuleID != "ping" {
		t.Fatalf("ping hit=%+v", hit)
	}
	if hit.DirectReply != "pong for ping test" {
		t.Fatalf("direct=%q", hit.DirectReply)
	}
}

func TestMatchContainsIsOr(t *testing.T) {
	cat := &Catalog{
		Rules: []Rule{{
			ID:    "hours",
			Match: Match{GoalContains: []string{"营业时间", "opening hours"}},
			DirectReply: "ok",
		}},
	}
	if err := cat.Compile(); err != nil {
		t.Fatal(err)
	}
	if cat.Match("请问营业时间？") == nil {
		t.Fatal("expected Chinese match")
	}
	if cat.Match("what are opening hours?") == nil {
		t.Fatal("expected English match")
	}
	if cat.Match("random") != nil {
		t.Fatal("expected no match")
	}
}

func TestMatchAllCriteria(t *testing.T) {
	cat := &Catalog{
		Rules: []Rule{{
			ID: "both",
			Match: Match{
				GoalPrefix:   "order:",
				GoalContains: []string{"123"},
			},
			DirectReply: "ok",
		}},
	}
	if err := cat.Compile(); err != nil {
		t.Fatal(err)
	}
	if cat.Match("order: fetch 123") == nil {
		t.Fatal("expected match")
	}
	if cat.Match("order: fetch 456") != nil {
		t.Fatal("contains should fail")
	}
	if cat.Match("help 123") != nil {
		t.Fatal("prefix should fail")
	}
}

func TestLoadCatalogValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		file string
		body string
		want string
	}{
		{"empty.json", `{"rules":[]}`, "empty rules"},
		{"dup.json", `{"rules":[{"id":"a","match":{"goal_prefix":"x"},"direct_reply":"y"},{"id":"a","match":{"goal_prefix":"z"},"direct_reply":"y"}]}`, "duplicate"},
		{"norole.json", `{"rules":[{"id":"a","match":{"goal_prefix":"x"},"messages":[{"role":"tool","content":"nope"}]}]}`, "unsupported role"},
		{"badre.json", `{"rules":[{"id":"a","match":{"goal_regex":"("},"direct_reply":"x"}]}`, "invalid goal_regex"},
	}
	for _, tc := range cases {
		write(tc.file, tc.body)
		_, err := LoadCatalog(filepath.Join(dir, tc.file))
		if err == nil {
			t.Fatalf("%s: expected error", tc.file)
		}
		if tc.want != "" && !contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v want substring %q", tc.file, err, tc.want)
		}
	}
}

func TestExpandMessagePreservesFields(t *testing.T) {
	m := expandMessage(model.Message{
		Role:    "assistant",
		Content: "goal={{goal}}",
		Name:    "demo",
	}, "hi")
	if m.Name != "demo" || m.Content != "goal=hi" {
		t.Fatalf("got %+v", m)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || len(sub) <= len(s) && findSub(s, sub)))
}

func findSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
