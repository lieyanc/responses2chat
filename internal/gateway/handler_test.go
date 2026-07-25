package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

func TestHandlerWithOfficialOpenAIGoClient(t *testing.T) {
	var upstreamAuth string
	var upstreamHeader string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		upstreamHeader = r.Header.Get("X-New-Api-Channel")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
          "id":"chatcmpl-sdk",
          "object":"chat.completion",
          "created":123,
          "model":"test-model",
          "choices":[{"index":0,"message":{"role":"assistant","content":"hello from chat"},"finish_reason":"stop"}],
          "usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}
        }`)
	}))
	defer upstream.Close()

	handler, err := New(Config{UpstreamURL: upstream.URL + "/v1/chat/completions"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	client := openai.NewClient(
		option.WithBaseURL(server.URL+"/v1"),
		option.WithAPIKey("managed-upstream-key"),
		option.WithHeader("X-New-Api-Channel", "channel-7"),
	)
	response, err := client.Responses.New(context.Background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel("test-model"),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String("hello"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputText() != "hello from chat" {
		t.Fatalf("official SDK output = %q", response.OutputText())
	}
	if upstreamAuth != "Bearer managed-upstream-key" {
		t.Fatalf("authorization was not passed through: %q", upstreamAuth)
	}
	if upstreamHeader != "channel-7" {
		t.Fatalf("custom end-to-end header was not passed through: %q", upstreamHeader)
	}
	if upstreamBody["model"] != "test-model" {
		t.Fatalf("upstream request = %#v", upstreamBody)
	}
}

func TestHandlerConvertsChatStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["stream"] != true {
			t.Fatalf("upstream stream flag = %#v", request["stream"])
		}
		options := request["stream_options"].(map[string]any)
		if options["include_usage"] != true {
			t.Fatalf("include_usage = %#v", options)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl-stream","created":100,"model":"stream-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-stream","created":100,"model":"stream-model","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`,
			`{"id":"chatcmpl-stream","created":100,"model":"stream-model","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-stream","created":100,"model":"stream-model","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`,
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	handler, err := New(Config{UpstreamURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"stream-model","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	text := string(body)
	for _, required := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		`"type":"response.output_text.delta"`,
		`"delta":"Hello "`,
		`"delta":"world"`,
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"total_tokens":4`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("stream missing %q:\n%s", required, text)
		}
	}
	assertMonotonicSequences(t, text)
}

func TestOfficialOpenAIGoClientDecodesStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-sdk-stream\",\"created\":100,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"SDK stream\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-sdk-stream\",\"created\":100,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	handler, _ := New(Config{UpstreamURL: upstream.URL})
	server := httptest.NewServer(handler)
	defer server.Close()

	client := openai.NewClient(
		option.WithBaseURL(server.URL+"/v1"),
		option.WithAPIKey("managed-key"),
	)
	stream := client.Responses.NewStreaming(context.Background(), responses.ResponseNewParams{
		Model: shared.ResponsesModel("m"),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String("hello"),
		},
	})
	var delta strings.Builder
	var completed bool
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.output_text.delta":
			delta.WriteString(event.AsResponseOutputTextDelta().Delta)
		case "response.completed":
			completed = true
			if event.AsResponseCompleted().Response.Usage.TotalTokens != 3 {
				t.Fatalf("SDK terminal usage = %#v", event.AsResponseCompleted().Response.Usage)
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if delta.String() != "SDK stream" || !completed {
		t.Fatalf("SDK stream delta=%q completed=%v", delta.String(), completed)
	}
}

func TestHandlerConvertsStreamingFunctionCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-tool\",\"created\":100,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-tool\",\"created\":100,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	handler, _ := New(Config{UpstreamURL: upstream.URL})
	server := httptest.NewServer(handler)
	defer server.Close()
	res, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	text := string(body)
	for _, required := range []string{
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"q\":1}"`,
		`"call_id":"call_1"`,
		"event: response.completed",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("function stream missing %q:\n%s", required, text)
		}
	}
}

func TestHandlerPreservesUpstreamJSONError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req_upstream")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error","param":null,"code":"rate_limit"}}`)
	}))
	defer upstream.Close()
	handler, _ := New(Config{UpstreamURL: upstream.URL})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-Id") != "req_upstream" || !strings.Contains(recorder.Body.String(), `"code":"rate_limit"`) {
		t.Fatalf("headers/body were not preserved: %#v %s", recorder.Header(), recorder.Body.String())
	}
}

func assertMonotonicSequences(t *testing.T, stream string) {
	t.Helper()
	next := int64(0)
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		sequence := int64(event["sequence_number"].(float64))
		if sequence != next {
			t.Fatalf("sequence=%d, want=%d", sequence, next)
		}
		next++
	}
}

func TestHandlerProxiesChatCompletionsVerbatim(t *testing.T) {
	requestBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],"some_future_field":{"nested":true}}`
	responseBody := `{"id":"chatcmpl-raw","object":"chat.completion","vendor_extension":123,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	var upstreamAuth, upstreamGot string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		upstreamAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		upstreamGot = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()
	handler, _ := New(Config{UpstreamURL: upstream.URL + "/v1/chat/completions"})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer chat-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamGot != requestBody {
		t.Fatalf("request body was not passed through verbatim: %s", upstreamGot)
	}
	if upstreamAuth != "Bearer chat-key" {
		t.Fatalf("authorization was not passed through: %q", upstreamAuth)
	}
	if recorder.Body.String() != responseBody {
		t.Fatalf("response body was not passed through verbatim: %s", recorder.Body.String())
	}
}

func TestHandlerProxiesChatCompletionsStreamVerbatim(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-raw","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"chatcmpl-raw","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			flusher.Flush()
		}
	}))
	defer upstream.Close()
	handler, _ := New(Config{UpstreamURL: upstream.URL})
	server := httptest.NewServer(handler)
	defer server.Close()

	res, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type = %q", res.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != strings.Join(chunks, "") {
		t.Fatalf("stream was not passed through verbatim:\n%s", body)
	}
}

func TestCopyEndToEndHeadersRemovesConnectionTokens(t *testing.T) {
	source := http.Header{
		"Authorization": []string{"Bearer secret"},
		"Connection":    []string{"keep-alive, X-Remove-Me"},
		"X-Remove-Me":   []string{"hop-only"},
	}
	destination := make(http.Header)
	copyEndToEndHeaders(destination, source)
	if destination.Get("Authorization") != "Bearer secret" {
		t.Fatalf("authorization was not copied: %#v", destination)
	}
	if destination.Get("Connection") != "" || destination.Get("X-Remove-Me") != "" {
		t.Fatalf("hop-by-hop headers leaked: %#v", destination)
	}
}

func TestHandlerRetriesWithoutUnsupportedParams(t *testing.T) {
	var attempts int
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, request)
		if _, ok := request["prompt_cache_key"]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Validation: Unsupported parameter(s): `+"`prompt_cache_key`"+`","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
	      "id":"chatcmpl-retry",
	      "object":"chat.completion",
	      "created":123,
	      "model":"test-model",
	      "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
	      "usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	    }`)
	}))
	defer upstream.Close()

	handler, err := New(Config{UpstreamURL: upstream.URL + "/v1/chat/completions", RetryUnsupportedParams: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Post(server.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi","prompt_cache_key":"cache-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if _, ok := bodies[1]["prompt_cache_key"]; ok {
		t.Fatalf("retry still contains prompt_cache_key: %#v", bodies[1])
	}
}

func TestHandlerRetryDisabledPassesErrorThrough(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Validation: Unsupported parameter(s): `+"`prompt_cache_key`"+`","type":"invalid_request_error"}}`)
	}))
	defer upstream.Close()

	handler, err := New(Config{UpstreamURL: upstream.URL + "/v1/chat/completions"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Post(server.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi","prompt_cache_key":"cache-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
