package gateway

import (
	"encoding/json"
	"testing"
)

func TestConvertResponseTextToolsUsageAndIncomplete(t *testing.T) {
	_, meta, err := convertRequest([]byte(`{
      "model":"test-model",
      "input":"hello",
      "max_output_tokens":5,
      "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]
    }`))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
      "id":"chatcmpl-abc",
      "object":"chat.completion",
      "created":123,
      "model":"actual-model",
      "choices":[{
        "index":0,
        "message":{
          "role":"assistant",
          "content":"partial",
          "annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":7}],
          "tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]
        },
        "finish_reason":"length",
        "logprobs":{"content":[{"token":"partial","logprob":-0.1,"top_logprobs":[]}]}
      }],
      "usage":{
        "prompt_tokens":10,
        "completion_tokens":5,
        "total_tokens":15,
        "prompt_tokens_details":{"cached_tokens":3},
        "completion_tokens_details":{"reasoning_tokens":2}
      }
    }`)
	got, err := convertResponse(body, meta)
	if err != nil {
		t.Fatal(err)
	}
	if got["id"] != "resp_abc" || got["status"] != "incomplete" {
		t.Fatalf("response identity/status = %#v / %#v", got["id"], got["status"])
	}
	details := got["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete reason = %#v", details)
	}
	output := got["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %#v", output)
	}
	text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if output[0].(map[string]any)["status"] != "incomplete" || output[1].(map[string]any)["status"] != "incomplete" {
		t.Fatalf("incomplete response items must be incomplete: %#v", output)
	}
	if text["text"] != "partial" || len(text["annotations"].([]any)) != 1 || text["logprobs"] == nil {
		t.Fatalf("text output = %#v", text)
	}
	call := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" {
		t.Fatalf("function output = %#v", call)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != int64(10) || usage["output_tokens"] != int64(5) {
		t.Fatalf("usage = %#v", usage)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}
