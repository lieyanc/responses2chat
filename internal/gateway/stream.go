package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type eventWriter struct {
	w        io.Writer
	flusher  http.Flusher
	sequence int64
}

func newEventWriter(w http.ResponseWriter) (*eventWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("response writer does not support streaming")
	}
	return &eventWriter{w: w, flusher: flusher}, nil
}

func (w *eventWriter) event(eventType string, data map[string]any) error {
	data["type"] = eventType
	data["sequence_number"] = w.sequence
	w.sequence++
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.w, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
		return err
	}
	w.flusher.Flush()
	return nil
}

type streamPart struct {
	kind         string
	contentIndex int
	text         string
	logprobs     []any
	added        bool
}

type streamMessage struct {
	id          string
	outputIndex int
	added       bool
	parts       map[string]*streamPart
	partOrder   []string
}

type streamTool struct {
	chatIndex    int
	id           string
	callID       string
	name         string
	arguments    string
	emittedBytes int
	outputIndex  int
	added        bool
}

type streamReasoning struct {
	id          string
	outputIndex int
	added       bool
	text        string
}

type streamState struct {
	meta         requestMeta
	writer       *eventWriter
	responseID   string
	created      int64
	model        string
	started      bool
	reasoning    *streamReasoning
	message      *streamMessage
	tools        map[int]*streamTool
	nextOutput   int
	finishReason string
	usage        any
	serviceTier  any
	failed       bool
}

func convertStream(w http.ResponseWriter, input io.Reader, meta requestMeta) error {
	events, err := newEventWriter(w)
	if err != nil {
		return err
	}
	state := &streamState{meta: meta, writer: events, tools: make(map[int]*streamTool)}
	err = readSSE(input, func(_ string, data string) error {
		if data == "[DONE]" {
			return io.EOF
		}
		chunk, err := decodeJSONObject(strings.NewReader(data))
		if err != nil {
			return fmt.Errorf("invalid upstream SSE JSON: %w", err)
		}
		if upstreamError, ok := object(chunk["error"]); ok {
			return state.fail(upstreamError)
		}
		return state.consume(chunk)
	})
	if err != nil && !errors.Is(err, io.EOF) {
		if state.failed {
			return err
		}
		_ = state.fail(map[string]any{
			"message": err.Error(),
			"type":    "upstream_error",
			"code":    "stream_error",
			"param":   nil,
		})
		return err
	}
	if !state.started {
		return state.fail(map[string]any{
			"message": "upstream stream ended before producing a response",
			"type":    "upstream_error",
			"code":    "empty_stream",
			"param":   nil,
		})
	}
	return state.finish()
}

func (s *streamState) consume(chunk map[string]any) error {
	if !s.started {
		s.responseID = responseID(stringValue(chunk["id"]))
		s.created = int64Number(chunk["created"])
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		s.model = stringValue(chunk["model"])
		if s.model == "" {
			s.model = stringValue(s.meta.original["model"])
		}
		s.started = true
		response := responseEnvelope(s.meta, s.responseID, s.created, s.model, "in_progress", []any{}, nil)
		if err := s.writer.event("response.created", map[string]any{"response": response}); err != nil {
			return err
		}
		if err := s.writer.event("response.in_progress", map[string]any{"response": response}); err != nil {
			return err
		}
	}
	if model := stringValue(chunk["model"]); model != "" {
		s.model = model
	}
	if usage := convertUsage(chunk["usage"]); usage != nil {
		s.usage = usage
	}
	if tier, ok := chunk["service_tier"]; ok && tier != nil {
		s.serviceTier = tier
	}

	choices, _ := chunk["choices"].([]any)
	for _, rawChoice := range choices {
		choice, ok := object(rawChoice)
		if !ok {
			continue
		}
		if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
			s.finishReason = finish
		}
		delta, _ := object(choice["delta"])
		if s.meta.reasoning {
			if reasoning := chatReasoningText(delta); reasoning != "" {
				if err := s.reasoningDelta(reasoning); err != nil {
					return err
				}
			}
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			if err := s.textDelta("output_text", content, choice["logprobs"]); err != nil {
				return err
			}
		}
		if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
			if err := s.textDelta("refusal", refusal, nil); err != nil {
				return err
			}
		}
		// Older Chat-compatible upstreams can still emit the deprecated
		// delta.function_call shape. Responses has only the modern function-call
		// Item, so normalize it into tool index zero.
		if legacy, ok := object(delta["function_call"]); ok {
			tool := s.ensureTool(0)
			if name, ok := legacy["name"].(string); ok {
				tool.name += name
			}
			if arguments, ok := legacy["arguments"].(string); ok {
				tool.arguments += arguments
			}
			if err := s.emitToolPending(tool); err != nil {
				return err
			}
		}
		toolCalls, _ := delta["tool_calls"].([]any)
		for _, rawCall := range toolCalls {
			call, ok := object(rawCall)
			if !ok {
				continue
			}
			index := int(int64Number(call["index"]))
			tool := s.ensureTool(index)
			if id, ok := call["id"].(string); ok && tool.callID == "" {
				tool.callID = id
			}
			function, _ := object(call["function"])
			if name, ok := function["name"].(string); ok && tool.name == "" {
				tool.name = name
			}
			if arguments, ok := function["arguments"].(string); ok {
				tool.arguments += arguments
			}
			if err := s.emitToolPending(tool); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) reasoningDelta(delta string) error {
	if s.reasoning == nil {
		s.reasoning = &streamReasoning{
			id:          newID("rs"),
			outputIndex: s.nextOutput,
		}
		s.nextOutput++
	}
	if !s.reasoning.added {
		s.reasoning.added = true
		if err := s.writer.event("response.output_item.added", map[string]any{
			"output_index": s.reasoning.outputIndex,
			"item": map[string]any{
				"id":      s.reasoning.id,
				"type":    "reasoning",
				"status":  "in_progress",
				"summary": []any{},
			},
		}); err != nil {
			return err
		}
		if err := s.writer.event("response.reasoning_summary_part.added", map[string]any{
			"item_id":       s.reasoning.id,
			"output_index":  s.reasoning.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		}); err != nil {
			return err
		}
	}
	s.reasoning.text += delta
	return s.writer.event("response.reasoning_summary_text.delta", map[string]any{
		"item_id":       s.reasoning.id,
		"output_index":  s.reasoning.outputIndex,
		"summary_index": 0,
		"delta":         delta,
	})
}

func (s *streamState) ensureMessage() (*streamMessage, error) {
	if s.message == nil {
		s.message = &streamMessage{
			id:          newID("msg"),
			outputIndex: s.nextOutput,
			parts:       make(map[string]*streamPart),
		}
		s.nextOutput++
	}
	if !s.message.added {
		s.message.added = true
		if err := s.writer.event("response.output_item.added", map[string]any{
			"output_index": s.message.outputIndex,
			"item": map[string]any{
				"id":      s.message.id,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		}); err != nil {
			return nil, err
		}
	}
	return s.message, nil
}

func (s *streamState) textDelta(kind, delta string, logprobs any) error {
	message, err := s.ensureMessage()
	if err != nil {
		return err
	}
	part := message.parts[kind]
	if part == nil {
		part = &streamPart{kind: kind, contentIndex: len(message.partOrder)}
		message.parts[kind] = part
		message.partOrder = append(message.partOrder, kind)
	}
	if !part.added {
		part.added = true
		payload := map[string]any{}
		if kind == "output_text" {
			payload = map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
		} else {
			payload = map[string]any{"type": "refusal", "refusal": ""}
		}
		if err := s.writer.event("response.content_part.added", map[string]any{
			"item_id":       message.id,
			"output_index":  message.outputIndex,
			"content_index": part.contentIndex,
			"part":          payload,
		}); err != nil {
			return err
		}
	}
	part.text += delta
	eventType := "response.output_text.delta"
	payload := map[string]any{
		"item_id":       message.id,
		"output_index":  message.outputIndex,
		"content_index": part.contentIndex,
		"delta":         delta,
	}
	if kind == "refusal" {
		eventType = "response.refusal.delta"
	} else {
		converted := streamLogprobs(logprobs)
		if converted == nil {
			converted = []any{}
		}
		if values, ok := converted.([]any); ok {
			part.logprobs = append(part.logprobs, values...)
		}
		payload["logprobs"] = converted
	}
	return s.writer.event(eventType, payload)
}

func (s *streamState) ensureTool(index int) *streamTool {
	if tool := s.tools[index]; tool != nil {
		return tool
	}
	tool := &streamTool{
		chatIndex:   index,
		id:          newID("fc"),
		outputIndex: s.nextOutput,
	}
	s.nextOutput++
	s.tools[index] = tool
	return tool
}

func (s *streamState) emitToolPending(tool *streamTool) error {
	if !tool.added && tool.name != "" {
		if tool.callID == "" {
			tool.callID = newID("call")
		}
		tool.added = true
		if err := s.writer.event("response.output_item.added", map[string]any{
			"output_index": tool.outputIndex,
			"item": map[string]any{
				"id":        tool.id,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   tool.callID,
				"name":      tool.name,
				"arguments": "",
			},
		}); err != nil {
			return err
		}
	}
	if tool.added && tool.emittedBytes < len(tool.arguments) {
		delta := tool.arguments[tool.emittedBytes:]
		tool.emittedBytes = len(tool.arguments)
		return s.writer.event("response.function_call_arguments.delta", map[string]any{
			"item_id":      tool.id,
			"output_index": tool.outputIndex,
			"delta":        delta,
		})
	}
	return nil
}

func (s *streamState) finish() error {
	status, incomplete := responseStatus(s.finishReason)
	itemStatus := "completed"
	if status == "incomplete" {
		itemStatus = "incomplete"
	}
	if s.message == nil && len(s.tools) == 0 && s.finishReason != "tool_calls" {
		if err := s.textDelta("output_text", "", nil); err != nil {
			return err
		}
	}
	if s.reasoning != nil && s.reasoning.added {
		if err := s.writer.event("response.reasoning_summary_text.done", map[string]any{
			"item_id":       s.reasoning.id,
			"output_index":  s.reasoning.outputIndex,
			"summary_index": 0,
			"text":          s.reasoning.text,
		}); err != nil {
			return err
		}
		if err := s.writer.event("response.reasoning_summary_part.done", map[string]any{
			"item_id":       s.reasoning.id,
			"output_index":  s.reasoning.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": s.reasoning.text},
		}); err != nil {
			return err
		}
		if err := s.writer.event("response.output_item.done", map[string]any{
			"output_index": s.reasoning.outputIndex,
			"item":         reasoningItem(s.reasoning.id, s.reasoning.text, itemStatus),
		}); err != nil {
			return err
		}
	}
	if s.message != nil {
		for _, kind := range s.message.partOrder {
			part := s.message.parts[kind]
			eventType := "response.output_text.done"
			content := map[string]any{"type": "output_text", "text": part.text, "annotations": []any{}}
			payload := map[string]any{
				"item_id":       s.message.id,
				"output_index":  s.message.outputIndex,
				"content_index": part.contentIndex,
				"text":          part.text,
			}
			if kind == "refusal" {
				eventType = "response.refusal.done"
				content = map[string]any{"type": "refusal", "refusal": part.text}
				delete(payload, "text")
				payload["refusal"] = part.text
			} else {
				payload["logprobs"] = part.logprobs
				content["logprobs"] = part.logprobs
			}
			if err := s.writer.event(eventType, payload); err != nil {
				return err
			}
			if err := s.writer.event("response.content_part.done", map[string]any{
				"item_id":       s.message.id,
				"output_index":  s.message.outputIndex,
				"content_index": part.contentIndex,
				"part":          content,
			}); err != nil {
				return err
			}
		}
		if err := s.writer.event("response.output_item.done", map[string]any{
			"output_index": s.message.outputIndex,
			"item":         s.messageItem(itemStatus),
		}); err != nil {
			return err
		}
	}
	for _, tool := range s.sortedTools() {
		if err := s.emitToolPending(tool); err != nil {
			return err
		}
		if !tool.added {
			continue
		}
		if err := s.writer.event("response.function_call_arguments.done", map[string]any{
			"item_id":      tool.id,
			"output_index": tool.outputIndex,
			"arguments":    tool.arguments,
		}); err != nil {
			return err
		}
		if err := s.writer.event("response.output_item.done", map[string]any{
			"output_index": tool.outputIndex,
			"item":         tool.item(itemStatus),
		}); err != nil {
			return err
		}
	}

	response := responseEnvelope(s.meta, s.responseID, s.created, s.model, status, s.finalOutput(itemStatus), s.usage)
	response["incomplete_details"] = incomplete
	if status == "completed" {
		response["completed_at"] = time.Now().Unix()
	}
	if s.serviceTier != nil {
		response["service_tier"] = s.serviceTier
	}
	eventType := "response.completed"
	if status == "incomplete" {
		eventType = "response.incomplete"
	}
	return s.writer.event(eventType, map[string]any{"response": response})
}

func (s *streamState) fail(upstream map[string]any) error {
	if s.failed {
		return errors.New(stringValue(upstream["message"]))
	}
	s.failed = true
	message := stringValue(upstream["message"])
	if message == "" {
		message = "upstream stream failed"
	}
	errorPayload := map[string]any{
		"code":    upstream["code"],
		"message": message,
		"param":   upstream["param"],
	}
	if err := s.writer.event("error", errorPayload); err != nil {
		return err
	}
	if !s.started {
		s.responseID = newID("resp")
		s.created = time.Now().Unix()
		s.model = stringValue(s.meta.original["model"])
		s.started = true
	}
	response := responseEnvelope(s.meta, s.responseID, s.created, s.model, "failed", s.finalOutput("incomplete"), s.usage)
	response["error"] = map[string]any{
		"code":    upstream["code"],
		"message": message,
	}
	if err := s.writer.event("response.failed", map[string]any{"response": response}); err != nil {
		return err
	}
	return errors.New(message)
}

func (s *streamState) messageItem(status string) map[string]any {
	content := make([]any, 0, len(s.message.partOrder))
	for _, kind := range s.message.partOrder {
		part := s.message.parts[kind]
		if kind == "refusal" {
			content = append(content, map[string]any{"type": "refusal", "refusal": part.text})
		} else {
			content = append(content, map[string]any{
				"type":        "output_text",
				"text":        part.text,
				"annotations": []any{},
				"logprobs":    part.logprobs,
			})
		}
	}
	return map[string]any{
		"id":      s.message.id,
		"type":    "message",
		"status":  status,
		"role":    "assistant",
		"content": content,
	}
}

func (t *streamTool) item(status string) map[string]any {
	return map[string]any{
		"id":        t.id,
		"type":      "function_call",
		"status":    status,
		"call_id":   t.callID,
		"name":      t.name,
		"arguments": t.arguments,
	}
}

func (s *streamState) finalOutput(status string) []any {
	type indexed struct {
		index int
		item  any
	}
	items := make([]indexed, 0, 2+len(s.tools))
	if s.reasoning != nil && s.reasoning.added {
		items = append(items, indexed{s.reasoning.outputIndex, reasoningItem(s.reasoning.id, s.reasoning.text, status)})
	}
	if s.message != nil {
		items = append(items, indexed{s.message.outputIndex, s.messageItem(status)})
	}
	for _, tool := range s.tools {
		if tool.added {
			items = append(items, indexed{tool.outputIndex, tool.item(status)})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].index < items[j].index })
	output := make([]any, 0, len(items))
	for _, item := range items {
		output = append(output, item.item)
	}
	return output
}

func (s *streamState) sortedTools() []*streamTool {
	tools := make([]*streamTool, 0, len(s.tools))
	for _, tool := range s.tools {
		tools = append(tools, tool)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].outputIndex < tools[j].outputIndex })
	return tools
}

func streamLogprobs(value any) any {
	logprobs, ok := object(value)
	if !ok {
		return nil
	}
	return logprobs["content"]
}

func readSSE(input io.Reader, consume func(event, data string) error) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	var event string
	var data bytes.Buffer
	flush := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		value := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		currentEvent := event
		event = ""
		return consume(currentEvent, value)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "event":
			event = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
