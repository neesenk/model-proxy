package protocol

import (
	sonic "github.com/bytedance/sonic"
)

// StreamTerminalComplete reports whether a buffered client-facing SSE stream
// reached its protocol's terminal success sequence:
//
//   - anthropic: a message_delta carrying a non-empty stop_reason, plus
//     message_stop. A bare message_stop (observed live from a kimi upstream
//     that abandoned a generation mid-thinking) is NOT complete.
//   - openai: a chunk with a non-null finish_reason, or a [DONE] terminator.
//   - responses: response.completed or response.incomplete.
//
// A stream carrying a terminal error frame (anthropic error event, openai
// error chunk, response.failed) is never complete. Callers must treat
// incomplete streams as truncated responses: safe to deliver (the bytes are
// what the upstream sent), never safe to cache for replay.
func StreamTerminalComplete(proto Protocol, body []byte) bool {
	events, err := parseWireSSE(body)
	if err != nil || len(events) == 0 {
		return false
	}
	switch proto {
	case Anthropic:
		stopReason, stop := false, false
		for _, ev := range events {
			var payload struct {
				Type  string `json:"type"`
				Delta struct {
					StopReason *string `json:"stop_reason"`
				} `json:"delta"`
			}
			if sonic.UnmarshalString(ev.data, &payload) != nil {
				continue
			}
			switch payload.Type {
			case "message_delta":
				if payload.Delta.StopReason != nil && *payload.Delta.StopReason != "" {
					stopReason = true
				}
			case "message_stop":
				stop = true
			case "error":
				return false
			}
		}
		return stopReason && stop
	case OpenAI:
		complete := false
		for _, ev := range events {
			if ev.data == "[DONE]" {
				complete = true
				continue
			}
			var payload struct {
				Error   any `json:"error"`
				Choices []struct {
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if sonic.UnmarshalString(ev.data, &payload) != nil {
				continue
			}
			if payload.Error != nil {
				return false
			}
			for _, choice := range payload.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					complete = true
				}
			}
		}
		return complete
	case Responses:
		for _, ev := range events {
			typ := ev.event
			var payload struct {
				Type string `json:"type"`
			}
			if sonic.UnmarshalString(ev.data, &payload) == nil && payload.Type != "" {
				typ = payload.Type
			}
			switch typ {
			case "response.completed", "response.incomplete":
				return true
			case "response.failed":
				return false
			}
		}
		return false
	default:
		return false
	}
}
