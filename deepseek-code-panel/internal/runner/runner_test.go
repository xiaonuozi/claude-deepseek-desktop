package runner

import "testing"

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
