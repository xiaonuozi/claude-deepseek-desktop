package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamParserAggregatesUsageByMessage(t *testing.T) {
	parser := newStreamParser()
	lines := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg-1","usage":{"input_tokens":10,"output_tokens":0}}},"session_id":"session-1"}`,
		`{"type":"assistant","message":{"id":"msg-1","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"assistant","message":{"id":"msg-2","usage":{"input_tokens":20,"output_tokens":7},"content":[{"type":"text","text":"world"}]}}`,
	}

	for _, line := range lines {
		parser.extract(line)
	}

	inputTokens, outputTokens := parser.usage()
	if inputTokens != 30 || outputTokens != 12 {
		t.Fatalf("expected token usage 30/12, got %d/%d", inputTokens, outputTokens)
	}
	if parser.sessionID != "session-1" {
		t.Fatalf("expected session id session-1, got %q", parser.sessionID)
	}
}

func TestStreamParserKeepsFinalUsageWhenMessageUsageIsPartial(t *testing.T) {
	parser := newStreamParser()
	lines := []string{
		`{"type":"message_start","message":{"id":"msg-1","usage":{"input_tokens":120,"output_tokens":0}}}`,
		`{"type":"result","usage":{"input_tokens":120,"output_tokens":42}}`,
	}

	for _, line := range lines {
		parser.extract(line)
	}

	inputTokens, outputTokens := parser.usage()
	if inputTokens != 120 || outputTokens != 42 {
		t.Fatalf("expected final usage 120/42, got %d/%d", inputTokens, outputTokens)
	}
}

func TestStreamParserDescribesToolTarget(t *testing.T) {
	parser := newStreamParser()
	eventType, display := parser.extract(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"Read","input":{"file_path":"internal/runner/runner.go"}}}`)
	if eventType != "tool-start" {
		t.Fatalf("expected tool-start, got %q", eventType)
	}
	if !strings.Contains(display, "internal/runner/runner.go") {
		t.Fatalf("expected tool target in display, got %q", display)
	}
}

func TestStreamParserHidesToolResultPayload(t *testing.T) {
	parser := newStreamParser()
	secret := strings.Repeat("secret-content", 2000)
	line, err := json.Marshal(map[string]interface{}{
		"type":       "user",
		"session_id": "session-tool",
		"message": map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content":     secret,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	eventType, display := parser.extract(string(line))
	if eventType != "tool-result" {
		t.Fatalf("expected tool-result, got %q", eventType)
	}
	if strings.Contains(display, "secret-content") {
		t.Fatalf("tool result payload leaked to frontend display")
	}
	if !strings.Contains(display, "工具结果已隐藏") {
		t.Fatalf("expected hidden payload summary, got %q", display)
	}
	if parser.sessionID != "session-tool" {
		t.Fatalf("expected session id to be recovered, got %q", parser.sessionID)
	}
	meta := parser.meta()
	if meta["content_hidden"] != true {
		t.Fatalf("expected content_hidden meta, got %#v", meta)
	}
}

func TestLimitFrontendDisplayChunkCapsTotal(t *testing.T) {
	counters := &runCounters{}
	first := strings.Repeat("a", maxFrontendDisplayChars-5)
	text, emit, capped := limitFrontendDisplayChunk(first, counters)
	if !emit || capped || text != first {
		t.Fatalf("expected first chunk to pass through, emit=%t capped=%t len=%d", emit, capped, len(text))
	}

	text, emit, capped = limitFrontendDisplayChunk(strings.Repeat("b", 100), counters)
	if !emit || !capped {
		t.Fatalf("expected cap note to be emitted once, emit=%t capped=%t", emit, capped)
	}
	if strings.Contains(text, strings.Repeat("b", 20)) {
		t.Fatalf("expected oversized display chunk to be hidden, got %q", text)
	}
	if !strings.Contains(text, "前端展示已达上限") {
		t.Fatalf("expected frontend cap note, got %q", text)
	}

	text, emit, capped = limitFrontendDisplayChunk("later output", counters)
	if emit || capped || text != "" {
		t.Fatalf("expected later chunks to be suppressed, emit=%t capped=%t text=%q", emit, capped, text)
	}
}

func TestStreamParserSuppressesLargeDuplicateFinalResult(t *testing.T) {
	parser := newStreamParser()
	parser.extract(`{"type":"message_start"}`)

	text := strings.Repeat("a", maxParserStateChars+1024)
	delta, err := json.Marshal(map[string]interface{}{
		"type": "content_block_delta",
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": text,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	eventType, display := parser.extract(string(delta))
	if eventType != "display" || display != text {
		t.Fatalf("expected streamed display delta, got type=%q len=%d", eventType, len(display))
	}

	result, err := json.Marshal(map[string]interface{}{
		"type":   "result",
		"result": text,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventType, display = parser.extract(string(result))
	if eventType != "" || display != "" {
		t.Fatalf("expected duplicate final result to be suppressed, got type=%q len=%d", eventType, len(display))
	}
	if warnings := strings.Join(parser.drainWarnings(), "\n"); !strings.Contains(warnings, "suppressed duplicate final assistant payload") {
		t.Fatalf("expected duplicate suppression warning, got %q", warnings)
	}
}

func TestStreamParserSkipsOversizedJSONLine(t *testing.T) {
	parser := newStreamParser()
	line := `{"type":"result","session_id":"session-heavy","result":"` + strings.Repeat("x", maxParserJSONLineChars+1) + `"}`

	eventType, display := parser.extract(line)
	if eventType != "" || display != "" {
		t.Fatalf("expected oversized line to be skipped, got type=%q len=%d", eventType, len(display))
	}
	if parser.sessionID != "session-heavy" {
		t.Fatalf("expected session id to be recovered, got %q", parser.sessionID)
	}
	if warnings := strings.Join(parser.drainWarnings(), "\n"); !strings.Contains(warnings, "skipped oversized JSON line") {
		t.Fatalf("expected oversized line warning, got %q", warnings)
	}
}

func TestStreamParserCapsThinkingMeta(t *testing.T) {
	parser := newStreamParser()
	parser.extract(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`)

	thinking := strings.Repeat("t", maxParserMetaChars+1024)
	delta, err := json.Marshal(map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{
			"type":     "thinking_delta",
			"thinking": thinking,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parser.extract(string(delta))
	eventType, _ := parser.extract(`{"type":"content_block_stop","index":0}`)
	if eventType != "thinking-end" {
		t.Fatalf("expected thinking-end, got %q", eventType)
	}
	meta := parser.meta()
	if _, ok := meta["content"]; ok {
		t.Fatalf("thinking meta should not include full content")
	}
	if meta["content_truncated"] != true {
		t.Fatalf("expected thinking meta to be marked truncated, got %#v", meta)
	}
}
