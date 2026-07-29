package agent

import (
	"context"
	"fmt"
	"strings"
)

// NewClientForProvider builds the right LLMClient for a saved AI config.
// Takes plain fields rather than a *db.AIConfig so this package doesn't
// depend on internal/db — callers (main.go, admin handlers) pass the
// config's fields straight through.
func NewClientForProvider(provider, apiKey, model, baseURL string) (LLMClient, error) {
	switch provider {
	case "gemini":
		return NewGeminiClient(apiKey, model), nil
	case "openai":
		return NewOpenAIClient(apiKey, model, baseURL), nil
	case "anthropic":
		return NewAnthropicClient(apiKey, model), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}

// TestConnection sends one minimal real request through client — no tools,
// no title-wrapping prompt — to confirm a saved config's key/model actually
// works. Used by the admin "Test" action, always against a throwaway
// client, never the shared active one.
func TestConnection(ctx context.Context, client LLMClient) error {
	content, _, err := client.Generate(ctx, []Content{{Role: "user", Parts: []Part{{Text: "Reply with just the word OK."}}}}, nil)
	if err != nil {
		return err
	}
	if content == nil || len(content.Parts) == 0 {
		return fmt.Errorf("empty response")
	}
	return nil
}

// schemaToJSON converts our Gemini-flavored Schema (uppercase Type values
// like "OBJECT"/"STRING") into a plain JSON Schema map, which is what both
// OpenAI's tools[].function.parameters and Anthropic's tools[].input_schema
// expect. Gemini's own adapter doesn't need this — it sends Schema as-is.
func schemaToJSON(s *Schema) map[string]interface{} {
	if s == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}

	out := map[string]interface{}{
		"type": strings.ToLower(s.Type),
	}
	if s.Description != "" {
		out["description"] = s.Description
	}
	if len(s.Properties) > 0 {
		props := make(map[string]interface{}, len(s.Properties))
		for name, prop := range s.Properties {
			props[name] = schemaToJSON(prop)
		}
		out["properties"] = props
	}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	}
	return out
}
