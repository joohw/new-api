package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/clovapi/switcher/internal/apistyle"
)

// EncodeNonStreamJSONResponse encodes ingress-shaped JSON completions for non-streaming proxy responses (Electron encodeResponseJson).
func EncodeNonStreamJSONResponseForStyle(ingress apistyle.Style, events []ResponseEvent) ([]byte, error) {
	switch ingressStyleForResponse(ingress) {
	case apistyle.Claude:
		return encodeResponseClaude(events)
	case apistyle.OpenAIChat:
		return encodeResponseOpenAIChat(events)
	case apistyle.OpenAIResponses:
		return encodeResponseOpenAIResponses(events)
	default:
		return nil, fmt.Errorf("unsupported ingress style for response encoding: %s", ingress)
	}
}

func ingressStyleForResponse(style apistyle.Style) apistyle.Style {
	if style == apistyle.Gemini {
		return apistyle.OpenAIChat
	}
	return style
}

func findError(events []ResponseEvent) (*ResponseEvent, bool) {
	for i := range events {
		if events[i].Type == RespError {
			return &events[i], true
		}
	}
	return nil, false
}

func foldText(events []ResponseEvent) (text string, finish string, model string, usageIn *ResponseEvent, hasFinish bool) {
	finish = "stop"
	model = ""
	for _, e := range events {
		switch e.Type {
		case RespTextDelta:
			text += e.Text
		case RespFinish:
			hasFinish = true
			if strings.TrimSpace(e.Reason) != "" {
				finish = strings.TrimSpace(e.Reason)
			}
		case RespMessageStart:
			if strings.TrimSpace(e.Model) != "" {
				model = strings.TrimSpace(e.Model)
			}
		case RespUsage:
			ev := e
			usageIn = &ev
		}
	}
	return text, finish, model, usageIn, hasFinish
}

// foldToolCalls reconstructs ordered tool calls from streaming-style events
// (start/delta/end), accumulating argument fragments per call id.
func foldToolCalls(events []ResponseEvent) []ToolCall {
	var calls []ToolCall
	pos := map[string]int{}
	current := ""
	for _, e := range events {
		switch e.Type {
		case RespToolStart:
			id := strings.TrimSpace(e.ID)
			if id == "" {
				id = fmt.Sprintf("call_%d", len(calls))
			}
			if _, ok := pos[id]; !ok {
				pos[id] = len(calls)
				calls = append(calls, ToolCall{ID: id, Name: strings.TrimSpace(e.Name)})
			}
			current = id
		case RespToolDelta:
			id := strings.TrimSpace(e.ID)
			if id == "" {
				id = current
			}
			if p, ok := pos[id]; ok {
				calls[p].Arguments += e.ArgsFragment
			}
		case RespToolEnd:
			current = ""
		}
	}
	return calls
}

func encodeResponseOpenAIChat(events []ResponseEvent) ([]byte, error) {
	if errEvt, ok := findError(events); ok {
		return json.Marshal(map[string]any{"error": errorOpenAIEnvelope(errEvt)})
	}
	text, finish, model, usage, _ := foldText(events)
	toolCalls := foldToolCalls(events)
	message := map[string]any{
		"role":    string(RoleAssistant),
		"content": text,
	}
	if len(toolCalls) > 0 {
		wire := make([]map[string]any, 0, len(toolCalls))
		for _, tc := range toolCalls {
			wire = append(wire, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": toolCallArgumentsOrEmpty(&tc),
				},
			})
		}
		message["tool_calls"] = wire
		if text == "" {
			message["content"] = nil
		}
		if f := strings.TrimSpace(strings.ToLower(finish)); f == "" || f == "stop" || f == "end_turn" || f == "completed" || f == "tool_use" {
			finish = "tool_calls"
		}
	}
	payload := map[string]any{
		"id":     "chatcmpl-proxy",
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishOpenAINormalize(finish),
			},
		},
	}
	if usage != nil {
		usagePayload := map[string]any{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		}
		if usage.HasCachedTokens {
			usagePayload["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
		}
		if usage.HasReasoningTokens {
			usagePayload["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.ReasoningTokens}
		}
		payload["usage"] = usagePayload
	}
	return json.Marshal(payload)
}

func finishOpenAINormalize(reason string) string {
	r := strings.TrimSpace(strings.ToLower(reason))
	switch r {
	case "completed", "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		if reason == "" {
			return "stop"
		}
		return strings.TrimSpace(reason)
	}
}

func errorOpenAIEnvelope(e *ResponseEvent) map[string]any {
	typ := strings.TrimSpace(e.Code)
	if typ == "" || typ == UpstreamErrorCode {
		typ = "api_error"
	}
	return map[string]any{"message": e.Message, "type": typ}
}

func encodeResponseOpenAIResponses(events []ResponseEvent) ([]byte, error) {
	if errEvt, ok := findError(events); ok {
		return json.Marshal(map[string]any{"error": errorOpenAIEnvelope(errEvt)})
	}
	text, _, _, usage, _ := foldText(events)
	output := []map[string]any{
		{
			"type": "message",
			"role": string(RoleAssistant),
			"content": []map[string]string{
				{"type": "output_text", "text": text},
			},
		},
	}
	for _, tc := range foldToolCalls(events) {
		output = append(output, map[string]any{
			"type":      "function_call",
			"call_id":   tc.ID,
			"name":      tc.Name,
			"arguments": toolCallArgumentsOrEmpty(&tc),
		})
	}
	payload := map[string]any{
		"id":     "resp_proxy",
		"object": "response",
		"status": "completed",
		"output": output,
	}
	if usage != nil {
		usagePayload := map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  usage.InputTokens + usage.OutputTokens,
		}
		if usage.HasCachedTokens {
			usagePayload["input_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
		}
		if usage.HasReasoningTokens {
			usagePayload["output_tokens_details"] = map[string]any{"reasoning_tokens": usage.ReasoningTokens}
		}
		payload["usage"] = usagePayload
	}
	return json.Marshal(payload)
}

func encodeResponseClaude(events []ResponseEvent) ([]byte, error) {
	if errEvt, ok := findError(events); ok {
		typ := strings.TrimSpace(errEvt.Code)
		if typ == "" {
			typ = "api_error"
		}
		return json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type": typ, "message": errEvt.Message,
			},
		})
	}

	text, finish, model, usage, _ := foldText(events)
	if strings.TrimSpace(finish) == "" {
		finish = "end_turn"
	}
	toolCalls := foldToolCalls(events)
	content := make([]map[string]any, 0, 1+len(toolCalls))
	if text != "" || len(toolCalls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for i := range toolCalls {
		content = append(content, claudeToolUseBlock(&toolCalls[i]))
	}
	stopReason := finishClaudeNormalize(finish)
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	payload := map[string]any{
		"id":          "msg_proxy",
		"type":        "message",
		"role":        string(RoleAssistant),
		"model":       model,
		"content":     content,
		"stop_reason": stopReason,
	}
	if usage != nil {
		usagePayload := map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		}
		if usage.HasCachedTokens {
			usagePayload["cache_read_input_tokens"] = usage.CachedTokens
		}
		payload["usage"] = usagePayload
	}
	return json.Marshal(payload)
}

func finishClaudeNormalize(reason string) string {
	switch strings.TrimSpace(strings.ToLower(reason)) {
	case "completed", "stop":
		return "end_turn"
	default:
		if strings.TrimSpace(reason) == "" {
			return "end_turn"
		}
		return strings.TrimSpace(reason)
	}
}
