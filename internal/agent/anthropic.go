// AnthropicClient implements LLMClient against Anthropic's Messages API
// (POST https://api.anthropic.com/v1/messages, headers x-api-key,
// anthropic-version, content-type).
//
// Wire format (verified against Anthropic's current docs): request tools
// are {name, description, input_schema}; max_tokens is required (this app
// has no concept of a token budget, so a fixed generous default is used).
// A tool call comes back as a content block {type:"tool_use", id, name,
// input}; the result must be sent back as a new *user*-role message whose
// content is a {type:"tool_result", tool_use_id, content} block — not a
// separate "tool" role like OpenAI. This maps cleanly onto this app's
// existing model→function→model loop (agent.go) because it already
// alternates one-for-one, which is exactly the assistant→user-with-
// tool_result alternation Anthropic requires.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// anthropicEndpoint is a var (not const) so tests can point it at a mock
// server; production code never reassigns it.
var anthropicEndpoint = "https://api.anthropic.com/v1/messages"

const (
	anthropicVersion   = "2023-06-01"
	anthropicMaxTokens = 2048
)

type AnthropicClient struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

func NewAnthropicClient(apiKey, model string) *AnthropicClient {
	return &AnthropicClient{
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type anthropicContentBlock struct {
	Type      string                 `json:"type"`
	Text      string                 `json:"text,omitempty"`
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	ToolUseID string                 `json:"tool_use_id,omitempty"`
	Content   string                 `json:"content,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (a *AnthropicClient) Generate(ctx context.Context, contents []Content, tools []Tool) (*Content, string, error) {
	messages := make([]anthropicMessage, 0, len(contents))
	for _, c := range contents {
		switch c.Role {
		case "function":
			var blocks []anthropicContentBlock
			for _, p := range c.Parts {
				if p.FunctionResponse == nil {
					continue
				}
				resultJSON, _ := json.Marshal(p.FunctionResponse.Response)
				blocks = append(blocks, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: p.FunctionResponse.ID,
					Content:   string(resultJSON),
				})
			}
			messages = append(messages, anthropicMessage{Role: "user", Content: blocks})
		default:
			role := "user"
			if c.Role == "model" {
				role = "assistant"
			}
			var blocks []anthropicContentBlock
			for _, p := range c.Parts {
				if p.Text != "" {
					blocks = append(blocks, anthropicContentBlock{Type: "text", Text: p.Text})
				}
				if p.FunctionCall != nil {
					blocks = append(blocks, anthropicContentBlock{
						Type:  "tool_use",
						ID:    p.FunctionCall.ID,
						Name:  p.FunctionCall.Name,
						Input: p.FunctionCall.Args,
					})
				}
			}
			messages = append(messages, anthropicMessage{Role: role, Content: blocks})
		}
	}

	var anthropicTools []anthropicTool
	if len(tools) > 0 {
		for _, decl := range tools[0].FunctionDeclarations {
			anthropicTools = append(anthropicTools, anthropicTool{
				Name:        decl.Name,
				Description: decl.Description,
				InputSchema: schemaToJSON(decl.Parameters),
			})
		}
	}

	reqBody := anthropicRequest{
		Model:     a.model,
		MaxTokens: anthropicMaxTokens,
		Messages:  messages,
		Tools:     anthropicTools,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, "", fmt.Errorf("unmarshal response: %w (body: %s)", err, string(body))
	}

	if ar.Error != nil {
		return nil, "", fmt.Errorf("anthropic API error (%s): %s", ar.Error.Type, ar.Error.Message)
	}

	var text string
	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "tool_use":
			return &Content{
				Role:  "model",
				Parts: []Part{{FunctionCall: &FunctionCall{ID: block.ID, Name: block.Name, Args: block.Input}}},
			}, ar.StopReason, nil
		}
	}

	return &Content{Role: "model", Parts: []Part{{Text: text}}}, ar.StopReason, nil
}
