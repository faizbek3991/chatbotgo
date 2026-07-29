package agent

import (
	"context"
	"fmt"
)

// maxSteps bounds how many function-call round trips one turn can take,
// so a confused model can't loop forever.
const maxSteps = 5

// StepLog records one tool call made during a turn, for display/audit.
type StepLog struct {
	ToolName string
	Args     map[string]interface{}
	Result   map[string]interface{}
}

// Result is what one call to Run produces: the final text answer plus a
// trace of every tool call the model made to get there.
type Result struct {
	FinalText string
	Steps     []StepLog
}

// Agent holds the tool registry/declarations only — it has no notion of a
// single "active" LLM client. Each user can be chatting through a different
// provider (their own key, or an approved shared one), so the caller
// resolves the right client per request (see ChatHandler.resolveClient) and
// passes it into Run explicitly.
type Agent struct {
	tools map[string]ToolFunc
	decls []FunctionDeclaration
}

func NewAgent() *Agent {
	return &Agent{
		tools: Registry(),
		decls: Declarations(),
	}
}

// Run executes the agentic loop for one turn against the given client: send
// the conversation so far, and if the model asks to call a tool, run it and
// send the result back — repeating until the model returns plain text
// instead of a function call.
//
// This is the "negotiation" from the original tutorial: the model can't
// call your function directly, it can only ask; your code decides whether
// and how to actually run it.
func (a *Agent) Run(ctx context.Context, history []Content, client LLMClient) (*Result, error) {
	contents := make([]Content, len(history))
	copy(contents, history)

	result := &Result{}

	for step := 0; step < maxSteps; step++ {
		modelContent, _, err := client.Generate(ctx, contents, []Tool{{FunctionDeclarations: a.decls}})
		if err != nil {
			return nil, err
		}

		var call *FunctionCall
		var text string
		for _, part := range modelContent.Parts {
			if part.FunctionCall != nil {
				call = part.FunctionCall
			}
			if part.Text != "" {
				text += part.Text
			}
		}

		if call == nil {
			// No function call: the model is done, this is the answer.
			result.FinalText = text
			return result, nil
		}

		// Echo the model's own function-call turn back into the history,
		// then run the tool and append its result as a "function" turn.
		contents = append(contents, Content{Role: "model", Parts: modelContent.Parts})

		fn, ok := a.tools[call.Name]
		if !ok {
			return nil, fmt.Errorf("model requested unknown tool %q", call.Name)
		}

		toolResult, err := fn(ctx, call.Args)
		if err != nil {
			toolResult = map[string]interface{}{"error": err.Error()}
		}

		result.Steps = append(result.Steps, StepLog{
			ToolName: call.Name,
			Args:     call.Args,
			Result:   toolResult,
		})

		contents = append(contents, Content{
			Role: "function",
			Parts: []Part{
				{FunctionResponse: &FunctionResponse{ID: call.ID, Name: call.Name, Response: toolResult}},
			},
		})
	}

	return nil, fmt.Errorf("agent exceeded %d steps without a final answer", maxSteps)
}
