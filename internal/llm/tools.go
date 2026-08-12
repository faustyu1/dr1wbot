// Package llm — tool-calling support.
//
// Chat is the raw entry point the tool loop uses: it sends a full message
// array with optional function definitions and returns whatever the model
// produced — plain text or a set of tool calls. Complete, by contrast, never
// sends tools and only returns text; the two exist so a handler that does not
// need tools pays nothing for them.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Tool is one function definition the model may call.
type Tool struct {
	Type     string  `json:"type"` // always "function"
	Function ToolDef `json:"function"`
}

// ToolDef describes a callable function to the model.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall is a function call the model requested.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction carries the name and raw-JSON arguments of one call.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Completion is the full result of one chat request: either a text answer or
// a set of tool calls the model wants executed. When ToolCalls is non-empty,
// Content is empty (or a short interjection the model made before calling).
type Completion struct {
	Content   string
	ToolCalls []ToolCall
}

// ChatMessage is one message in a raw conversation. It covers all roles,
// including "tool" results, which the Prompt/History/System triplet in Request
// cannot express.
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall // assistant messages that request functions
	ToolCallID string     // tool messages: matches the call's id
	Name       string     // optional function name for tool messages
}

// ChatRequest is a raw chat request with full control over messages.
type ChatRequest struct {
	Messages  []ChatMessage
	Tools     []Tool
	MaxTokens int
}

// ErrNoToolsReturned is returned when the model produced neither text nor tool
// calls — an empty answer that is not a refusal and not a length truncation.
var ErrNoToolsReturned = errors.New("model returned neither text nor tool calls")

// Chat sends a raw chat request and returns the completion. It walks models ×
// keys the same way Complete does, so a spent quota on one model falls through
// to the next. The difference is what it extracts from the response: Content
// when the model answered, ToolCalls when it asked for a function.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (Completion, error) {
	if len(c.models) == 0 {
		return Completion{}, fmt.Errorf("no model configured")
	}
	if c.keys.Len() == 0 {
		return Completion{}, fmt.Errorf("no api key configured")
	}

	maxTokens := c.maxTokens
	if req.MaxTokens > 0 && req.MaxTokens < maxTokens {
		maxTokens = req.MaxTokens
	}

	// Build the message array once; it does not change across model/key
	// attempts because tools and messages are the same regardless of which
	// model answers.
	messages := make([]message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = make([]toolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				msg.ToolCalls[i] = toolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: toolCallFunc{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}
			// An assistant message with tool_calls sends content as null.
			if m.Content == "" {
				msg.Content = nil
			}
		}
		messages = append(messages, msg)
	}

	tools := make([]toolDef, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, toolDef{
			Type: t.Type,
			Function: toolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}

	var lastErr error
	quotaHit := false

	for _, model := range c.models {
		payload, err := json.Marshal(completionRequest{
			Model:           model,
			Messages:        messages,
			MaxTokens:       maxTokens,
			ReasoningEffort: c.reasoningEffort,
			Stream:          false,
			Tools:           tools,
		})
		if err != nil {
			return Completion{}, fmt.Errorf("encode request: %w", err)
		}

		for _, lease := range c.keys.Lease() {
			parsed, err := c.callRaw(ctx, model, lease.Key, payload)
			if err == nil {
				c.keys.Works(lease.Index)
				c.recordAnswer(model)

				choice := parsed.Choices[0]
				out := Completion{
					Content:   choice.Message.Content,
					ToolCalls: convertToolCalls(choice.Message.ToolCalls),
				}
				if out.Content == "" && len(out.ToolCalls) == 0 {
					if refusal := choice.Message.Refusal; refusal != "" {
						return Completion{}, fmt.Errorf("model refused: %s", refusal)
					}
					return Completion{}, ErrNoToolsReturned
				}
				return out, nil
			}
			lastErr = err

			switch {
			case errors.Is(err, ErrRateLimited):
				c.keys.Limit(lease.Index)
				quotaHit = true
			case errors.Is(err, errOverloaded):
				// Another key may land on a healthier backend.
			default:
				c.recordFailure(false)
				return Completion{}, err
			}
			if ctx.Err() != nil {
				c.recordFailure(false)
				return Completion{}, lastErr
			}
		}
	}

	c.recordFailure(quotaHit)
	if quotaHit {
		return Completion{}, errors.Join(ErrRateLimited, lastErr)
	}
	return Completion{}, lastErr
}

// convertToolCalls lifts the unexported wire type into the exported one.
func convertToolCalls(calls []toolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(calls))
	for i, tc := range calls {
		out[i] = ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: ToolFunction{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		}
	}
	return out
}

// BuildMessages constructs a ChatMessage slice from the same inputs Complete
// uses — system prompt, history turns, and the user prompt — so the tool loop
// can start from the same shape a plain answer would, then add tool exchanges
// on top.
func BuildMessages(system string, history []Turn, prompt string) []ChatMessage {
	msgs := make([]ChatMessage, 0, len(history)+2)
	if system != "" {
		msgs = append(msgs, ChatMessage{Role: "system", Content: system})
	}
	for _, t := range history {
		if t.Content == "" {
			continue
		}
		msgs = append(msgs, ChatMessage{Role: t.Role, Content: t.Content})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: prompt})
	return msgs
}
