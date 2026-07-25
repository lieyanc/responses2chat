package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func convertResponse(body []byte, meta requestMeta) (map[string]any, error) {
	chat, err := decodeJSONObject(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid upstream JSON response: %w", err)
	}
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return nil, errors.New("upstream response contains no choices")
	}
	choice, ok := object(choices[0])
	if !ok {
		return nil, errors.New("upstream response contains an invalid choice")
	}
	message, _ := object(choice["message"])
	finishReason, _ := choice["finish_reason"].(string)
	status, incomplete := responseStatus(finishReason)
	itemStatus := "completed"
	if status == "incomplete" {
		itemStatus = "incomplete"
	}
	output := convertChatMessage(message, choice, itemStatus, meta.reasoning)

	created := int64Number(chat["created"])
	if created == 0 {
		created = time.Now().Unix()
	}
	model, _ := chat["model"].(string)
	if model == "" {
		model, _ = meta.original["model"].(string)
	}
	response := responseEnvelope(meta, responseID(stringValue(chat["id"])), created, model, status, output, convertUsage(chat["usage"]))
	response["incomplete_details"] = incomplete
	if status == "completed" {
		response["completed_at"] = time.Now().Unix()
	}
	if serviceTier, ok := chat["service_tier"]; ok {
		response["service_tier"] = serviceTier
	}
	return response, nil
}

func convertChatMessage(message, choice map[string]any, itemStatus string, reasoningPassthrough bool) []any {
	output := make([]any, 0)
	if reasoningPassthrough {
		if text := chatReasoningText(message); text != "" {
			output = append(output, reasoningItem(newID("rs"), text, itemStatus))
		}
	}
	contentParts := make([]any, 0, 2)
	if content, ok := message["content"].(string); ok {
		part := map[string]any{
			"type":        "output_text",
			"text":        content,
			"annotations": annotations(message["annotations"]),
		}
		if logprobs := responseLogprobs(choice["logprobs"]); logprobs != nil {
			part["logprobs"] = logprobs
		}
		contentParts = append(contentParts, part)
	}
	if refusal, ok := message["refusal"].(string); ok && refusal != "" {
		contentParts = append(contentParts, map[string]any{
			"type":    "refusal",
			"refusal": refusal,
		})
	}
	if len(contentParts) > 0 {
		output = append(output, map[string]any{
			"id":      newID("msg"),
			"type":    "message",
			"status":  itemStatus,
			"role":    "assistant",
			"content": contentParts,
		})
	}

	toolCalls, _ := message["tool_calls"].([]any)
	for _, raw := range toolCalls {
		call, ok := object(raw)
		if !ok || call["type"] != "function" {
			continue
		}
		function, _ := object(call["function"])
		name, _ := function["name"].(string)
		arguments, _ := function["arguments"].(string)
		callID, _ := call["id"].(string)
		if name == "" || callID == "" {
			continue
		}
		output = append(output, functionCallOutput(callID, name, arguments, itemStatus))
	}
	if legacy, ok := object(message["function_call"]); ok {
		name, _ := legacy["name"].(string)
		arguments, _ := legacy["arguments"].(string)
		if name != "" {
			output = append(output, functionCallOutput(newID("call"), name, arguments, itemStatus))
		}
	}
	return output
}

func chatReasoningText(message map[string]any) string {
	if text, ok := message["reasoning_content"].(string); ok && text != "" {
		return text
	}
	if text, ok := message["reasoning"].(string); ok && text != "" {
		return text
	}
	return ""
}

func reasoningItem(id, text, status string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "reasoning",
		"status": status,
		"summary": []any{
			map[string]any{"type": "summary_text", "text": text},
		},
	}
}

func functionCallOutput(callID, name, arguments, status string) map[string]any {
	return map[string]any{
		"id":        newID("fc"),
		"type":      "function_call",
		"status":    status,
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	}
}

func responseEnvelope(meta requestMeta, id string, created int64, model, status string, output []any, usage any) map[string]any {
	original := meta.original
	response := map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           created,
		"status":               status,
		"background":           false,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nullable(original, "instructions"),
		"max_output_tokens":    nullable(original, "max_output_tokens"),
		"max_tool_calls":       nil,
		"model":                model,
		"output":               output,
		"parallel_tool_calls":  defaultValue(original, "parallel_tool_calls", true),
		"previous_response_id": nil,
		"reasoning":            responseReasoning(original["reasoning"]),
		"store":                false,
		"temperature":          nullable(original, "temperature"),
		"text":                 responseText(original["text"]),
		"tool_choice":          responseToolChoice(meta),
		"tools":                meta.appliedTools,
		"top_logprobs":         nullable(original, "top_logprobs"),
		"top_p":                nullable(original, "top_p"),
		"truncation":           "disabled",
		"usage":                usage,
		"user":                 nullable(original, "user"),
		"metadata":             defaultValue(original, "metadata", map[string]any{}),
	}
	return response
}

func responseReasoning(value any) map[string]any {
	reasoning, _ := object(value)
	return map[string]any{
		"effort":  nullable(reasoning, "effort"),
		"summary": nil,
	}
}

func responseText(value any) map[string]any {
	text, _ := object(value)
	result := map[string]any{"format": map[string]any{"type": "text"}}
	if format, ok := object(text["format"]); ok {
		formatType, _ := format["type"].(string)
		switch formatType {
		case "text", "json_object", "json_schema":
			result["format"] = format
		}
	}
	if verbosity, ok := text["verbosity"]; ok {
		result["verbosity"] = verbosity
	}
	return result
}

func responseToolChoice(meta requestMeta) any {
	if meta.appliedToolChoice != nil {
		return meta.appliedToolChoice
	}
	return "auto"
}

func convertUsage(value any) any {
	usage, ok := object(value)
	if !ok {
		return nil
	}
	promptDetails, _ := object(usage["prompt_tokens_details"])
	completionDetails, _ := object(usage["completion_tokens_details"])
	return map[string]any{
		"input_tokens": int64Number(usage["prompt_tokens"]),
		"input_tokens_details": map[string]any{
			"cached_tokens":      int64Number(promptDetails["cached_tokens"]),
			"cache_write_tokens": int64Number(promptDetails["cache_write_tokens"]),
		},
		"output_tokens": int64Number(usage["completion_tokens"]),
		"output_tokens_details": map[string]any{
			"reasoning_tokens": int64Number(completionDetails["reasoning_tokens"]),
		},
		"total_tokens": int64Number(usage["total_tokens"]),
	}
}

func responseLogprobs(value any) any {
	logprobs, ok := object(value)
	if !ok {
		return nil
	}
	content, ok := logprobs["content"].([]any)
	if !ok {
		return nil
	}
	return content
}

func annotations(value any) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{}
	}
	return items
}

func responseStatus(reason string) (string, any) {
	switch reason {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

func nullable(values map[string]any, key string) any {
	if values == nil {
		return nil
	}
	return values[key]
}

func defaultValue(values map[string]any, key string, fallback any) any {
	if values != nil {
		if value, ok := values[key]; ok && value != nil {
			return value
		}
	}
	return fallback
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func int64Number(value any) int64 {
	switch number := value.(type) {
	case json.Number:
		result, _ := number.Int64()
		return result
	case float64:
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	default:
		return 0
	}
}
