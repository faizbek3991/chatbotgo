// OpenAIClient implements LLMClient against the OpenAI Chat Completions API
// (POST {base_url}/chat/completions, Authorization: Bearer <key>). The same
// adapter also serves any OpenAI-compatible endpoint (Groq, Together,
// local Ollama, etc.) by pointing baseURL elsewhere — the request/response
// shape is identical.
//
// Wire format (verified against OpenAI's current docs): request tools are
// {type:"function", function:{name, description, parameters}}; a returned
// function call is a tool_calls[] entry {id, type:"function",
// function:{name, arguments: "<json string>"}}; the result is sent back as
// a new message {role:"tool", tool_call_id, content}. Unlike Gemini, OpenAI
// requires that id to correlate a call with its result — see the ID field
// on FunctionCall/FunctionResponse in gemini.go.
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

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

type OpenAIClient struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
}

func NewOpenAIClient(apiKey, model, baseURL string) *OpenAIClient {
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	return &OpenAIClient{
		apiKey:     apiKey,
		model:      model,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	} `json:"function"`
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []openAITool    `json:"tools,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func (o *OpenAIClient) Generate(ctx context.Context, contents []Content, tools []Tool) (*Content, string, error) {
	messages := make([]openAIMessage, 0, len(contents))
	for _, c := range contents {
		switch c.Role {
		case "function":
			for _, p := range c.Parts {
				if p.FunctionResponse == nil {
					continue
				}
				resultJSON, _ := json.Marshal(p.FunctionResponse.Response)
				messages = append(messages, openAIMessage{
					Role:       "tool",
					ToolCallID: p.FunctionResponse.ID,
					Content:    string(resultJSON),
				})
			}
		default:
			role := "user"
			if c.Role == "model" {
				role = "assistant"
			}
			msg := openAIMessage{Role: role}
			for _, p := range c.Parts {
				if p.Text != "" {
					msg.Content += p.Text
				}
				if p.FunctionCall != nil {
					argsJSON, _ := json.Marshal(p.FunctionCall.Args)
					tc := openAIToolCall{ID: p.FunctionCall.ID, Type: "function"}
					tc.Function.Name = p.FunctionCall.Name
					tc.Function.Arguments = string(argsJSON)
					msg.ToolCalls = append(msg.ToolCalls, tc)
				}
			}
			messages = append(messages, msg)
		}
	}

	var openAITools []openAITool
	if len(tools) > 0 {
		for _, decl := range tools[0].FunctionDeclarations {
			t := openAITool{Type: "function"}
			t.Function.Name = decl.Name
			t.Function.Description = decl.Description
			t.Function.Parameters = schemaToJSON(decl.Parameters)
			openAITools = append(openAITools, t)
		}
	}

	reqBody := openAIRequest{Model: o.model, Messages: messages, Tools: openAITools}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.httpClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("openai request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	var or openAIResponse
	if err := json.Unmarshal(body, &or); err != nil {
		return nil, "", fmt.Errorf("unmarshal response: %w (body: %s)", err, string(body))
	}

	if or.Error != nil {
		return nil, "", fmt.Errorf("openai API error (%s): %s", or.Error.Type, or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return nil, "", fmt.Errorf("openai returned no choices (http %d): %s", resp.StatusCode, string(body))
	}

	choice := or.Choices[0]
	if len(choice.Message.ToolCalls) > 0 {
		tc := choice.Message.ToolCalls[0]
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return nil, "", fmt.Errorf("parse tool call arguments: %w", err)
		}
		return &Content{
			Role:  "model",
			Parts: []Part{{FunctionCall: &FunctionCall{ID: tc.ID, Name: tc.Function.Name, Args: args}}},
		}, choice.FinishReason, nil
	}

	text := choice.Message.Content
	if text == "" {
		// Some providers (e.g. DeepSeek via SumoPod/litellm) put the actual
		// answer in reasoning_content and leave content empty. Replaying an
		// empty-content, no-tool-call assistant turn back as history is
		// rejected by those same providers ("content or tool_calls must be
		// set"), so never store a truly empty turn.
		text = choice.Message.ReasoningContent
	}
	if text == "" {
		return nil, "", fmt.Errorf("openai response had no content or tool call (finish_reason=%s)", choice.FinishReason)
	}

	return &Content{Role: "model", Parts: []Part{{Text: text}}}, choice.FinishReason, nil
}
