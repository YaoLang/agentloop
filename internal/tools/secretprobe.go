package tools

import (
	"context"
	"encoding/json"
)

// HasSecretTool reports whether a named per-tenant secret is configured.
// The observation is only "present" or "absent" — never the value.
// It is not a Default builtin; register it via Options.Extra / ExtraTools
// (tests) or copy the pattern for outbound-HTTP tools.
func HasSecretTool() *Tool {
	return &Tool{
		Name:        "has_secret",
		Description: "Return present or absent for a named tenant secret. Never returns the secret value.",
		Schema: map[string]any{
			"type":     "object",
			"required": required("name"),
			"properties": map[string]any{
				"name": prop("string"),
			},
		},
		Handler: func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", err
			}
			rt, ok := RuntimeFrom(ctx)
			if !ok {
				return "absent", nil
			}
			_, present := rt.lookupSecret(a.Name)
			if present {
				return "present", nil
			}
			return "absent", nil
		},
	}
}
