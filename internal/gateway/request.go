package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type requestMeta struct {
	original          map[string]any
	appliedTools      []any
	appliedToolChoice any
	stream            bool
	reasoning         bool
}

func convertRequest(body []byte, reasoningPassthrough bool) (map[string]any, requestMeta, error) {
	src, err := decodeJSONObject(bytes.NewReader(body))
	if err != nil {
		return nil, requestMeta{}, fmt.Errorf("invalid JSON request: %w", err)
	}

	dst := make(map[string]any)
	meta := requestMeta{original: src, reasoning: reasoningPassthrough}
	copyFields(src, dst,
		"model", "parallel_tool_calls", "temperature", "top_p",
		"prompt_cache_key", "safety_identifier", "user", "service_tier", "metadata",
	)
	// Chat's store flag cannot implement Responses object retrieval or
	// previous_response_id chaining, especially because response IDs are rewritten.
	// Disable storage explicitly instead of advertising state the gateway cannot use.
	dst["store"] = false

	messages := make([]any, 0)
	if instructions, ok := src["instructions"].(string); ok {
		messages = append(messages, map[string]any{
			"role":    "developer",
			"content": instructions,
		})
	}
	if input, ok := src["input"]; ok && input != nil {
		converted, err := convertInput(input, reasoningPassthrough)
		if err != nil {
			return nil, requestMeta{}, err
		}
		messages = append(messages, converted...)
	}
	dst["messages"] = messages

	if value, ok := src["max_output_tokens"]; ok {
		dst["max_completion_tokens"] = value
	}
	if topLogprobs, ok := src["top_logprobs"]; ok {
		dst["top_logprobs"] = topLogprobs
		dst["logprobs"] = true
	}
	if reasoning, ok := object(src["reasoning"]); ok {
		if effort, ok := reasoning["effort"]; ok && effort != nil {
			dst["reasoning_effort"] = effort
		}
	}
	if text, ok := object(src["text"]); ok {
		if verbosity, ok := text["verbosity"]; ok && verbosity != nil {
			dst["verbosity"] = verbosity
		}
		if format, ok := object(text["format"]); ok {
			if converted, ok := convertTextFormat(format); ok {
				dst["response_format"] = converted
			}
		}
	}

	tools := convertTools(src["tools"])
	if len(tools.chat) > 0 {
		dst["tools"] = tools.chat
	}
	meta.appliedTools = tools.responses
	choice, choiceOK := convertToolChoice(src["tool_choice"], len(tools.chat) > 0)
	if choiceOK {
		dst["tool_choice"] = choice.chat
		meta.appliedToolChoice = choice.responses
	}

	if stream, _ := src["stream"].(bool); stream {
		meta.stream = true
		dst["stream"] = true
		// Responses requires usage in its terminal event. Chat streams only send it
		// when explicitly requested.
		dst["stream_options"] = map[string]any{"include_usage": true}
	}

	return dst, meta, nil
}

func copyFields(src, dst map[string]any, fields ...string) {
	for _, field := range fields {
		if value, ok := src[field]; ok && value != nil {
			dst[field] = value
		}
	}
}

func object(value any) (map[string]any, bool) {
	v, ok := value.(map[string]any)
	return v, ok
}

func convertInput(input any, reasoningPassthrough bool) ([]any, error) {
	switch value := input.(type) {
	case string:
		return []any{map[string]any{"role": "user", "content": value}}, nil
	case []any:
		return convertItems(value, reasoningPassthrough), nil
	default:
		return nil, errors.New("input must be a string or an array of input items")
	}
}

func convertItems(items []any, reasoningPassthrough bool) []any {
	messages := make([]any, 0, len(items))
	lastAssistant := -1
	pendingReasoning := ""

	attachReasoning := func(assistant map[string]any) {
		if pendingReasoning == "" {
			return
		}
		if assistant["reasoning_content"] == nil {
			assistant["reasoning_content"] = pendingReasoning
		}
		pendingReasoning = ""
	}

	for _, raw := range items {
		item, ok := object(raw)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		if itemType == "" && item["role"] != nil {
			itemType = "message"
		}

		switch itemType {
		case "message":
			message, ok := convertMessage(item)
			if !ok {
				continue
			}
			messages = append(messages, message)
			if message["role"] == "assistant" {
				lastAssistant = len(messages) - 1
				attachReasoning(message)
			} else {
				lastAssistant = -1
				pendingReasoning = ""
			}
		case "function_call":
			call, ok := convertFunctionCall(item)
			if !ok {
				continue
			}
			if lastAssistant < 0 {
				assistant := map[string]any{
					"role":       "assistant",
					"tool_calls": []any{call},
				}
				attachReasoning(assistant)
				messages = append(messages, assistant)
				lastAssistant = len(messages) - 1
			} else {
				assistant := messages[lastAssistant].(map[string]any)
				calls, _ := assistant["tool_calls"].([]any)
				assistant["tool_calls"] = append(calls, call)
				attachReasoning(assistant)
			}
		case "function_call_output":
			message, ok := convertFunctionOutput(item)
			if !ok {
				continue
			}
			messages = append(messages, message)
			lastAssistant = -1
			pendingReasoning = ""
		case "reasoning":
			if !reasoningPassthrough {
				continue
			}
			// A reasoning Item precedes the assistant message or function call it
			// belongs to; hold its text and attach it to that assistant message as
			// the non-standard reasoning_content field understood by reasoning-model
			// Chat upstreams.
			if text := reasoningItemText(item); text != "" {
				pendingReasoning = text
			}
		default:
			// Hosted/MCP/computer/shell/image-generation calls, item references,
			// approvals and any future Items have no faithful Chat Completions
			// message representation. They are intentionally dropped.
		}
	}
	return messages
}

func reasoningItemText(item map[string]any) string {
	collect := func(value any, partType, field string) string {
		parts, _ := value.([]any)
		texts := make([]string, 0, len(parts))
		for _, raw := range parts {
			part, ok := object(raw)
			if !ok || part["type"] != partType {
				continue
			}
			if text, ok := part[field].(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n")
	}
	if text := collect(item["content"], "reasoning_text", "text"); text != "" {
		return text
	}
	return collect(item["summary"], "summary_text", "text")
}

func convertMessage(item map[string]any) (map[string]any, bool) {
	role, _ := item["role"].(string)
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		return nil, false
	}

	content, ok := convertContent(item["content"], role)
	if !ok {
		return nil, false
	}
	message := map[string]any{"role": role, "content": content}
	if name, ok := item["name"].(string); ok && name != "" {
		message["name"] = name
	}
	return message, true
}

func convertContent(value any, role string) (any, bool) {
	if text, ok := value.(string); ok {
		return text, true
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, false
	}

	converted := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := object(raw)
		if !ok {
			continue
		}
		partType, _ := part["type"].(string)
		switch partType {
		case "input_text", "output_text", "text":
			if text, ok := part["text"].(string); ok {
				converted = append(converted, map[string]any{"type": "text", "text": text})
			}
		case "refusal":
			if role == "assistant" {
				if refusal, ok := part["refusal"].(string); ok {
					converted = append(converted, map[string]any{"type": "refusal", "refusal": refusal})
				}
			}
		case "input_image", "image_url":
			if role != "user" {
				continue
			}
			if image := convertImagePart(part); image != nil {
				converted = append(converted, image)
			}
		case "input_file", "file":
			if role != "user" {
				continue
			}
			if file := convertFilePart(part); file != nil {
				converted = append(converted, file)
			}
		}
	}
	if len(converted) == 0 {
		return nil, false
	}
	return converted, true
}

func convertImagePart(part map[string]any) map[string]any {
	if nested, ok := object(part["image_url"]); ok {
		url, _ := nested["url"].(string)
		if url == "" {
			return nil
		}
		imageURL := map[string]any{"url": url}
		if detail, ok := nested["detail"].(string); ok && detail != "" {
			imageURL["detail"] = detail
		}
		return map[string]any{"type": "image_url", "image_url": imageURL}
	}
	url, _ := part["image_url"].(string)
	if url == "" {
		// Chat Completions cannot address a Responses image by file_id.
		return nil
	}
	imageURL := map[string]any{"url": url}
	if detail, ok := part["detail"].(string); ok && detail != "" {
		imageURL["detail"] = detail
	}
	return map[string]any{"type": "image_url", "image_url": imageURL}
}

func convertFilePart(part map[string]any) map[string]any {
	if nested, ok := object(part["file"]); ok {
		part = nested
	}
	file := make(map[string]any)
	copyFields(part, file, "file_id", "file_data", "filename")
	if file["file_id"] == nil && file["file_data"] == nil {
		// file_url is valid in Responses, but Chat Completions has no equivalent.
		return nil
	}
	return map[string]any{"type": "file", "file": file}
}

func convertFunctionCall(item map[string]any) (map[string]any, bool) {
	name, _ := item["name"].(string)
	arguments, _ := item["arguments"].(string)
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID, _ = item["id"].(string)
	}
	if name == "" || callID == "" {
		return nil, false
	}
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}, true
}

func convertFunctionOutput(item map[string]any) (map[string]any, bool) {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		return nil, false
	}
	content := convertToolOutput(item["output"])
	return map[string]any{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      content,
	}, true
}

func convertToolOutput(value any) any {
	if text, ok := value.(string); ok {
		return text
	}
	parts, ok := value.([]any)
	if !ok {
		if value == nil {
			return ""
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
	converted := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := object(raw)
		if !ok {
			continue
		}
		partType, _ := part["type"].(string)
		if partType == "input_text" || partType == "text" {
			if text, ok := part["text"].(string); ok {
				converted = append(converted, map[string]any{"type": "text", "text": text})
			}
		}
	}
	if len(converted) == 0 {
		return ""
	}
	return converted
}

type convertedTools struct {
	chat      []any
	responses []any
}

func convertTools(value any) convertedTools {
	items, _ := value.([]any)
	result := convertedTools{
		chat:      make([]any, 0, len(items)),
		responses: make([]any, 0, len(items)),
	}
	for _, raw := range items {
		tool, ok := object(raw)
		if !ok || tool["type"] != "function" {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		definition := map[string]any{"name": name}
		copyFields(tool, definition, "description", "parameters", "strict")
		result.chat = append(result.chat, map[string]any{
			"type":     "function",
			"function": definition,
		})
		responseTool := map[string]any{"type": "function", "name": name}
		copyFields(tool, responseTool, "description", "parameters", "strict")
		result.responses = append(result.responses, responseTool)
	}
	return result
}

type convertedChoice struct {
	chat      any
	responses any
}

func convertToolChoice(value any, hasTools bool) (convertedChoice, bool) {
	if mode, ok := value.(string); ok {
		switch mode {
		case "none":
			return convertedChoice{chat: mode, responses: mode}, true
		case "auto", "required":
			if !hasTools {
				return convertedChoice{chat: "none", responses: "none"}, true
			}
			return convertedChoice{chat: mode, responses: mode}, true
		}
		return convertedChoice{}, false
	}
	choice, ok := object(value)
	if !ok || !hasTools || choice["type"] != "function" {
		return convertedChoice{}, false
	}
	name, _ := choice["name"].(string)
	if name == "" {
		return convertedChoice{}, false
	}
	return convertedChoice{
		chat: map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		},
		responses: map[string]any{"type": "function", "name": name},
	}, true
}

func convertTextFormat(format map[string]any) (map[string]any, bool) {
	formatType, _ := format["type"].(string)
	switch formatType {
	case "", "text":
		return nil, false
	case "json_object":
		return map[string]any{"type": "json_object"}, true
	case "json_schema":
		schema := make(map[string]any)
		copyFields(format, schema, "name", "description", "schema", "strict")
		if schema["name"] == nil || schema["schema"] == nil {
			return nil, false
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}, true
	default:
		return nil, false
	}
}
