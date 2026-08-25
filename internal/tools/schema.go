package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateArgs checks args against a small JSON Schema subset:
// type=object, required[], properties.{name: {type}}.
func ValidateArgs(schema map[string]any, argsJSON string) error {
	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("tool args: not a JSON object: %w", err)
	}
	if schema == nil {
		return nil
	}
	if t, _ := schema["type"].(string); t != "" && t != "object" {
		return fmt.Errorf("schema type %q not supported", t)
	}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if name == "" {
				continue
			}
			if _, exists := args[name]; !exists {
				return fmt.Errorf("missing required argument %q", name)
			}
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for name, raw := range args {
		spec, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		want, _ := spec["type"].(string)
		if want == "" {
			continue
		}
		if err := checkType(want, raw); err != nil {
			return fmt.Errorf("argument %q: %w", name, err)
		}
	}
	return nil
}

func checkType(want string, v any) error {
	switch want {
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("want string")
		}
	case "number":
		switch v.(type) {
		case float64, json.Number:
		default:
			return fmt.Errorf("want number")
		}
	case "integer":
		switch n := v.(type) {
		case float64:
			if n != float64(int(n)) {
				return fmt.Errorf("want integer")
			}
		case json.Number:
		default:
			return fmt.Errorf("want integer")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("want boolean")
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("want array")
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("want object")
		}
	}
	return nil
}

func required(names ...string) []any {
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

func prop(typ string) map[string]any { return map[string]any{"type": typ} }
