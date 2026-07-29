// Package agent implements the agentic loop: send the conversation plus
// tool declarations to Gemini, execute whatever function call comes back,
// and repeat until the model returns plain text.
//
// This talks to the Gemini REST API directly (net/http + encoding/json)
// rather than through the Go SDK. That keeps the project dependency-light
// and makes the actual wire format — the part that matters for
// understanding function calling — visible and readable in one file.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"

// --- Canonical wire types, modeled on Gemini's JSON shape (functionCall /
// functionResponse parts) since that's the format this app was built
// around first. Other providers (openai.go, anthropic.go) translate their
// own, genuinely different function-calling shapes to/from these at their
// boundary — see LLMClient.
//
// ID is only meaningful for providers that need to correlate a function
// call with its result (OpenAI's tool_call_id, Anthropic's tool_use_id).
// Gemini doesn't use one, so its own marshal/unmarshal simply never
// populates or reads this field.

type Part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

type FunctionCall struct {
	ID   string                 `json:"id,omitempty"`
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type FunctionResponse struct {
	ID       string                 `json:"id,omitempty"`
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

// Schema is a (deliberately small) subset of the OpenAPI-style schema
// Gemini expects for function parameters.
type Schema struct {
	Type        string             `json:"type"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
}

type FunctionDeclaration struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Parameters  *Schema `json:"parameters,omitempty"`
}

type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations"`
}

// LLMClient is what Agent and GenerateTitle depend on — implemented by
// GeminiClient here, and by OpenAIClient/AnthropicClient (openai.go,
// anthropic.go), each translating this canonical shape to/from its own
// provider's wire format.
type LLMClient interface {
	Generate(ctx context.Context, contents []Content, tools []Tool) (*Content, string, error)
}

type generateRequest struct {
	Contents []Content `json:"contents"`
	Tools    []Tool    `json:"tools,omitempty"`
}

type generateResponse struct {
	Candidates []struct {
		Content      Content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// GeminiClient implements LLMClient for Gemini's REST API. Its fields are
// immutable after construction — switching providers/keys/models now means
// building a new LLMClient via NewClientForConfig and swapping it in at the
// Agent level (Agent.SetClient), not mutating a client in place.
type GeminiClient struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

func NewGeminiClient(apiKey, model string) *GeminiClient {
	return &GeminiClient{
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Generate sends the conversation so far plus the available tools to
// Gemini and returns the model's response content (which may contain a
// functionCall part, a text part, or both).
func (g *GeminiClient) Generate(ctx context.Context, contents []Content, tools []Tool) (*Content, string, error) {
	reqBody := generateRequest{Contents: contents, Tools: tools}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf(geminiEndpoint, g.model, g.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	var gr generateResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, "", fmt.Errorf("unmarshal response: %w (body: %s)", err, string(body))
	}

	if gr.Error != nil {
		return nil, "", fmt.Errorf("gemini API error %d (%s): %s", gr.Error.Code, gr.Error.Status, gr.Error.Message)
	}
	if len(gr.Candidates) == 0 {
		return nil, "", fmt.Errorf("gemini returned no candidates (http %d): %s", resp.StatusCode, string(body))
	}

	cand := gr.Candidates[0]
	return &cand.Content, cand.FinishReason, nil
}

// GenerateTitle asks Gemini for a short chat title summarizing a
// conversation's first message. It's a plain one-off completion — no tools
// are offered, so the response is always text, never a functionCall.
func GenerateTitle(ctx context.Context, client LLMClient, firstMessage string) (string, error) {
	prompt := fmt.Sprintf(
		"Write a short chat title (3-6 words, no quotes, no trailing punctuation) summarizing this message:\n\n%s",
		firstMessage,
	)
	content, _, err := client.Generate(ctx, []Content{{Role: "user", Parts: []Part{{Text: prompt}}}}, nil)
	if err != nil {
		return "", err
	}
	if len(content.Parts) == 0 {
		return "", fmt.Errorf("empty title response")
	}
	title := strings.TrimSpace(content.Parts[0].Text)
	title = strings.Trim(title, "\"'")
	if title == "" {
		return "", fmt.Errorf("empty title response")
	}
	return title, nil
}
