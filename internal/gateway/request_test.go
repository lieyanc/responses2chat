package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestConvertRequestPreservesCompatibleContext(t *testing.T) {
	body := []byte(`{
  "model": "test-model",
  "instructions": "top-level guidance",
  "input": [
    {"role":"system","content":[{"type":"input_text","text":"system context"}]},
    {"role":"developer","content":"developer context"},
    {"role":"user","content":[
      {"type":"input_text","text":"look"},
      {"type":"input_image","image_url":"https://example.com/a.png","detail":"high"},
      {"type":"input_file","file_id":"file_123","filename":"a.pdf"},
      {"type":"input_file","file_url":"https://example.com/unsupported.pdf"}
    ]},
    {"type":"reasoning","id":"rs_1","summary":[]},
    {"type":"message","role":"assistant","content":[
      {"type":"output_text","text":"I will call it"},
      {"type":"refusal","refusal":"not this part"}
    ]},
    {"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"},
    {"type":"function_call","id":"fc_2","call_id":"call_2","name":"lookup","arguments":"{\"q\":\"y\"}"},
    {"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"result"}]},
    {"type":"mcp_call","id":"mcp_1"}
  ],
  "max_output_tokens": 321,
  "reasoning": {"effort":"high","summary":"auto"},
  "text": {"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true},"verbosity":"low"},
  "tools": [
    {"type":"web_search_preview"},
    {"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"},"strict":true}
  ],
  "tool_choice": {"type":"function","name":"lookup"},
  "top_logprobs": 2,
	"store": true,
  "stream": true,
  "previous_response_id": "resp_unsupported",
  "conversation": "conv_unsupported",
  "background": true
}`)

	got, meta, err := convertRequest(body, false)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.stream {
		t.Fatal("stream flag was not preserved")
	}
	if got["max_completion_tokens"] != json.Number("321") {
		t.Fatalf("max token mapping = %#v", got["max_completion_tokens"])
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning effort = %#v", got["reasoning_effort"])
	}
	if got["verbosity"] != "low" {
		t.Fatalf("verbosity = %#v", got["verbosity"])
	}
	if got["logprobs"] != true || got["top_logprobs"] != json.Number("2") {
		t.Fatalf("logprobs mapping = %#v / %#v", got["logprobs"], got["top_logprobs"])
	}
	if got["store"] != false {
		t.Fatalf("Responses storage must be disabled, got %#v", got["store"])
	}
	for _, dropped := range []string{"previous_response_id", "conversation", "background", "reasoning", "text", "input", "instructions"} {
		if _, exists := got[dropped]; exists {
			t.Errorf("unsupported/source field %q leaked upstream", dropped)
		}
	}

	messages := got["messages"].([]any)
	if len(messages) != 6 {
		t.Fatalf("got %d messages: %#v", len(messages), messages)
	}
	if messages[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("instructions were not prepended: %#v", messages[0])
	}
	userParts := messages[3].(map[string]any)["content"].([]any)
	if len(userParts) != 3 {
		t.Fatalf("unsupported file URL should be dropped, parts=%#v", userParts)
	}
	assistant := messages[4].(map[string]any)
	toolCalls := assistant["tool_calls"].([]any)
	if len(toolCalls) != 2 {
		t.Fatalf("parallel calls were not grouped into assistant message: %#v", assistant)
	}
	toolResult := messages[5].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_1" {
		t.Fatalf("function output mapping = %#v", toolResult)
	}

	tools := got["tools"].([]any)
	if len(tools) != 1 || len(meta.appliedTools) != 1 {
		t.Fatalf("only function tools should survive: %#v", tools)
	}
	wantChoice := map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}
	if !reflect.DeepEqual(got["tool_choice"], wantChoice) {
		t.Fatalf("tool choice = %#v", got["tool_choice"])
	}
	streamOptions := got["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream usage was not forced: %#v", streamOptions)
	}
}

func TestConvertRequestDropsUnsupportedOnly(t *testing.T) {
	got, meta, err := convertRequest([]byte(`{
      "model":"test",
      "input":[
        {"type":"reasoning","id":"r"},
        {"type":"item_reference","id":"i"},
        {"role":"user","content":[{"type":"input_image","file_id":"file_image"}]}
      ],
      "tools":[{"type":"file_search","vector_store_ids":["vs"]}],
      "tool_choice":"required"
    }`), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["messages"].([]any)) != 0 {
		t.Fatalf("unsupported context leaked: %#v", got["messages"])
	}
	if _, ok := got["tools"]; ok {
		t.Fatalf("unsupported tools leaked: %#v", got["tools"])
	}
	if got["tool_choice"] != "none" || meta.appliedToolChoice != "none" {
		t.Fatalf("required unsupported tools must degrade to none: %#v", got["tool_choice"])
	}
}

func TestConvertRequestRejectsTrailingJSON(t *testing.T) {
	if _, _, err := convertRequest([]byte(`{"model":"m","input":"x"} {"second":true}`), false); err == nil {
		t.Fatal("multiple JSON values must be rejected")
	}
}

func TestConvertRequestReasoningPassthrough(t *testing.T) {
	body := []byte(`{
  "model": "test-model",
  "input": [
    {"role":"user","content":"hi"},
    {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking about it"}]},
    {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
    {"type":"function_call_output","call_id":"call_1","output":"result"},
    {"type":"reasoning","id":"rs_2","content":[{"type":"reasoning_text","text":"raw thoughts"}]},
    {"type":"message","role":"assistant","content":"done"}
  ]
}`)

	got, _, err := convertRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	messages := got["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	toolCaller := messages[1].(map[string]any)
	if toolCaller["reasoning_content"] != "thinking about it" {
		t.Fatalf("tool-call reasoning_content = %#v", toolCaller["reasoning_content"])
	}
	final := messages[3].(map[string]any)
	if final["reasoning_content"] != "raw thoughts" {
		t.Fatalf("final reasoning_content = %#v", final["reasoning_content"])
	}

	off, _, err := convertRequest(body, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range off["messages"].([]any) {
		if raw.(map[string]any)["reasoning_content"] != nil {
			t.Fatal("reasoning_content leaked with passthrough disabled")
		}
	}
}
