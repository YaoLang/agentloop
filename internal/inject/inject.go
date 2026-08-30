// Package inject matches run goals against tenant rules and turns configured
// context into model.Message slices to improve one-round direct answers.
package inject

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/YaoLang/agentloop/internal/model"
)

const goalVar = "{{goal}}"

var allowedRoles = map[string]bool{
	"system":    true,
	"user":      true,
	"assistant": true,
}

// Match selects goals for a rule. All non-empty criteria must pass.
type Match struct {
	GoalPrefix   string   `json:"goal_prefix,omitempty"`
	GoalContains []string `json:"goal_contains,omitempty"`
	GoalRegex    string   `json:"goal_regex,omitempty"`
}

// Rule is one injectable context entry. The first matching rule wins.
type Rule struct {
	ID          string          `json:"id"`
	Match       Match           `json:"match"`
	Messages    []model.Message `json:"messages,omitempty"`
	DirectReply string          `json:"direct_reply,omitempty"`
}

type compiledRule struct {
	Rule
	regex *regexp.Regexp
}

// Catalog is data/tenants/{id}/context.json (outside workspace/).
type Catalog struct {
	Rules []Rule `json:"rules"`
	rules []compiledRule
}

// Hit is the outcome of matching a goal against a catalog.
type Hit struct {
	RuleID      string
	Messages    []model.Message
	DirectReply string
}

// LoadCatalog reads and validates a context catalog file.
// Missing file returns the os.IsNotExist error from ReadFile.
func LoadCatalog(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cat Catalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, fmt.Errorf("context catalog: invalid json")
	}
	if err := cat.compile(); err != nil {
		return nil, err
	}
	return &cat, nil
}

// Compile validates in-memory rules (for tests and programmatic catalogs).
func (c *Catalog) Compile() error {
	return c.compile()
}

func (c *Catalog) compile() error {
	if c == nil {
		return fmt.Errorf("context catalog: empty")
	}
	if len(c.Rules) == 0 {
		return fmt.Errorf("context catalog: empty rules")
	}
	c.rules = make([]compiledRule, 0, len(c.Rules))
	seen := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		r.ID = strings.TrimSpace(r.ID)
		if r.ID == "" {
			return fmt.Errorf("context catalog: empty rule id")
		}
		if seen[r.ID] {
			return fmt.Errorf("context catalog: duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		if !r.Match.hasCriteria() {
			return fmt.Errorf("context catalog: rule %q has no match criteria", r.ID)
		}
		hasMessages := len(r.Messages) > 0
		hasDirect := strings.TrimSpace(r.DirectReply) != ""
		if !hasMessages && !hasDirect {
			return fmt.Errorf("context catalog: rule %q needs messages or direct_reply", r.ID)
		}
		if hasMessages {
			for j, m := range r.Messages {
				role := strings.TrimSpace(m.Role)
				if !allowedRoles[role] {
					return fmt.Errorf("context catalog: rule %q message %d: unsupported role %q", r.ID, j, m.Role)
				}
				r.Messages[j].Role = role
			}
		}
		cr := compiledRule{Rule: *r}
		if re := strings.TrimSpace(r.Match.GoalRegex); re != "" {
			rx, err := regexp.Compile(re)
			if err != nil {
				return fmt.Errorf("context catalog: rule %q: invalid goal_regex", r.ID)
			}
			cr.regex = rx
		}
		c.rules = append(c.rules, cr)
	}
	return nil
}

func (m Match) hasCriteria() bool {
	if strings.TrimSpace(m.GoalPrefix) != "" {
		return true
	}
	for _, s := range m.GoalContains {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return strings.TrimSpace(m.GoalRegex) != ""
}

// Match returns the first matching rule for goal, or nil.
func (c *Catalog) Match(goal string) *Hit {
	if c == nil {
		return nil
	}
	for _, r := range c.rules {
		if !r.Match.matches(goal, r.regex) {
			continue
		}
		hit := &Hit{RuleID: r.ID}
		if dr := strings.TrimSpace(r.DirectReply); dr != "" {
			hit.DirectReply = expand(dr, goal)
			return hit
		}
		hit.Messages = make([]model.Message, len(r.Messages))
		for i, m := range r.Messages {
			hit.Messages[i] = expandMessage(m, goal)
		}
		return hit
	}
	return nil
}

func (m Match) matches(goal string, rx *regexp.Regexp) bool {
	if strings.TrimSpace(m.GoalPrefix) != "" {
		if !strings.HasPrefix(strings.ToLower(goal), strings.ToLower(strings.TrimSpace(m.GoalPrefix))) {
			return false
		}
	}
	if subs := nonEmptyStrings(m.GoalContains); len(subs) > 0 {
		matched := false
		lower := strings.ToLower(goal)
		for _, sub := range subs {
			if strings.Contains(lower, strings.ToLower(sub)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if rx != nil && !rx.MatchString(goal) {
		return false
	}
	return true
}

func nonEmptyStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func expand(s, goal string) string {
	return strings.ReplaceAll(s, goalVar, goal)
}

func expandMessage(m model.Message, goal string) model.Message {
	out := m
	out.Content = expand(m.Content, goal)
	return out
}
