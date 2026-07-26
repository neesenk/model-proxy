package main

// convert_differential_test.go — differential tests against opencodex
// (~/code/opencodex) behavior: input fixtures under testdata/differential/
// are wire-format SSE streams lifted VERBATIM from opencodex's test files and
// fed to our converters. Assertions check semantic equivalence; where we
// deliberately diverge (intentional-behaviors.md), we assert OUR semantics
// and cite the entry number. Fixture provenance:
//
//	anthropic_signature_delta.sse   ← tests/anthropic-thinking-signature.test.ts ("signature_delta on a thinking block yields thinking_signature")
//	anthropic_signature_stray.sse   ← tests/anthropic-thinking-signature.test.ts ("signature_delta outside a thinking block is ignored")
//	anthropic_redacted.sse          ← tests/anthropic-thinking-signature.test.ts ("redacted_thinking blocks surface with their opaque data")
//	chat_parallel_interleaved.sse   ← tests/openai-chat-parallel-stream.test.ts (T1)
//	chat_idonly_continuation.sse    ← tests/openai-chat-parallel-stream.test.ts (T9)
//	chat_eof_truncated.sse          ← tests/openai-chat-eof.test.ts ("truncated stream yields a terminal error")
//	chat_eof_no_newline.sse         ← tests/openai-chat-eof.test.ts ("final frame WITHOUT a trailing newline")
//	chat_content_filter.sse         ← tests/openai-chat-eof.test.ts ("EOF carries content_filter through the bridge as incomplete")

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readDifferential(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/differential", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// opencodex captures signature_delta as thinking_signature; its bridge then
// packs it into an ocxr1 envelope. We map it directly to the reasoning item's
// encrypted_content (our contract: signature ↔ encrypted_content, verbatim) —
// same payload, different envelope format.
func TestDifferential_AnthropicSignatureDelta(t *testing.T) {
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(readDifferential(t, "anthropic_signature_delta.sse")), "claude-x"))
	dones := sseFilter(events, "response.output_item.done")
	if len(dones) != 1 {
		t.Fatalf("done items = %d: %v", len(dones), sseEventTypes(events))
	}
	item := asMap(sseDataMap(t, dones[0])["item"])
	if item["type"] != "reasoning" {
		t.Errorf("done item type = %v, want reasoning", item["type"])
	}
	// The done-part item carries the accumulated summary + verbatim signature.
	parts := sseFilter(events, "response.reasoning_summary_part.done")
	if len(parts) != 1 {
		t.Fatalf("reasoning_summary_part.done = %d", len(parts))
	}
	pItem := asMap(sseDataMap(t, parts[0])["item"])
	if pItem["encrypted_content"] != "AbCdEf1234567890sig==" {
		t.Errorf("encrypted_content = %v, want verbatim signature", pItem["encrypted_content"])
	}
	if strOf(asMap(asSlice(pItem["summary"], 0))["text"]) != "let me think" {
		t.Errorf("summary = %v", pItem["summary"])
	}
}

// opencodex: "signature_delta outside a thinking block is ignored
// (block-scoped)". Parity: no encrypted_content leaks anywhere.
func TestDifferential_AnthropicStraySignature(t *testing.T) {
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(readDifferential(t, "anthropic_signature_stray.sse")), "claude-x"))
	for _, ev := range events {
		if ev.data == "[DONE]" {
			continue
		}
		if strings.Contains(ev.data, "encrypted_content") || strings.Contains(ev.data, "StraySignature123456") {
			t.Errorf("stray signature leaked into output frame: %s", ev.data)
		}
	}
	// The text block still closes normally.
	if got := sseCount(events, "response.completed"); got != 1 {
		t.Errorf("response.completed = %d, want 1", got)
	}
}

// opencodex surfaces redacted_thinking with its opaque data. We map it to a
// reasoning item carrying ONLY encrypted_content (data verbatim, no summary).
func TestDifferential_AnthropicRedacted(t *testing.T) {
	events := drainSSE(t, newAnthropicToResponsesSSE(strings.NewReader(readDifferential(t, "anthropic_redacted.sse")), "claude-x"))
	assertResponsesItemPairing(t, events)
	added := sseFilter(events, "response.output_item.added")
	if len(added) != 1 {
		t.Fatalf("added items = %d: %v", len(added), sseEventTypes(events))
	}
	item := asMap(sseDataMap(t, added[0])["item"])
	if item["type"] != "reasoning" || item["encrypted_content"] != "OPAQUE1" {
		t.Errorf("redacted reasoning item = %v", item)
	}
	if got := sseCount(events, "response.reasoning_summary_text.delta"); got != 0 {
		t.Errorf("redacted block must not emit summary deltas, got %d", got)
	}
}

// opencodex T1: interleaved index-keyed deltas assemble without
// cross-contamination; done arguments reconcile per call.
func TestDifferential_ChatParallelInterleaved(t *testing.T) {
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(readDifferential(t, "chat_parallel_interleaved.sse")), "gpt-x"))
	assertResponsesItemPairing(t, events)
	dones := sseFilter(events, "response.function_call_arguments.done")
	if len(dones) != 2 {
		t.Fatalf("args done = %d: %v", len(dones), sseEventTypes(events))
	}
	got := map[string]string{}
	for _, d := range dones {
		m := sseDataMap(t, d)
		got[strOf(m["item_id"])] = strOf(m["arguments"])
	}
	if got["call_a"] != `{"cmd":"ls"}` || got["call_b"] != `{"path":"a.txt"}` {
		t.Errorf("assembled arguments = %v, want call_a=%s call_b=%s", got, `{"cmd":"ls"}`, `{"path":"a.txt"}`)
	}
	if got := sseCount(events, "response.completed"); got != 1 {
		t.Errorf("response.completed = %d", got)
	}
}

// opencodex T9: an index+id first chunk followed by an id-only continuation
// (no index) stays ONE call. Our tool map keys by index, and a missing index
// decodes as 0 — same call, parity.
func TestDifferential_ChatIDOnlyContinuation(t *testing.T) {
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(readDifferential(t, "chat_idonly_continuation.sse")), "gpt-x"))
	if got := sseCount(events, "response.output_item.added"); got != 1 {
		t.Fatalf("added items = %d, want 1 (id-only continuation stays one call): %v", got, sseEventTypes(events))
	}
	dones := sseFilter(events, "response.function_call_arguments.done")
	if len(dones) != 1 || strOf(sseDataMap(t, dones[0])["arguments"]) != `{"cmd":"ls"}` {
		t.Fatalf("done args = %v", dones)
	}
}

// A truncated stream must fail closed, matching opencodex: no clean terminal
// may be synthesized from partial text or tool arguments.
func TestDifferential_ChatEOFTruncated(t *testing.T) {
	in := readDifferential(t, "chat_eof_truncated.sse")
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	if got := sseCount(events, "response.completed"); got != 0 {
		t.Errorf("chat→r truncated EOF: response.completed = %d, want 0", got)
	}
	if got := sseCount(events, "response.failed"); got != 1 {
		t.Errorf("chat→r truncated EOF: response.failed = %d, want 1", got)
	}
	eventsA := drainSSE(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	if got := sseCount(eventsA, "message_stop"); got != 0 {
		t.Errorf("chat→a truncated EOF: message_stop = %d, want 0", got)
	}
	if got := sseCount(eventsA, "error"); got != 1 {
		t.Errorf("chat→a truncated EOF: error = %d, want 1", got)
	}
}

// opencodex: a final frame WITHOUT a trailing newline still emits its content
// and is accepted as done. Parity (bufio.Scanner yields the final token).
func TestDifferential_ChatEOFNoNewline(t *testing.T) {
	in := readDifferential(t, "chat_eof_no_newline.sse")
	if strings.HasSuffix(in, "\n") {
		t.Fatal("fixture must lack the trailing newline")
	}
	eventsA := drainSSE(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	foundText := false
	for _, ev := range sseFilter(eventsA, "content_block_delta") {
		if strOf(asMap(sseDataMap(t, ev)["delta"])["text"]) == "hi" {
			foundText = true
		}
	}
	if !foundText {
		t.Errorf("final-frame content dropped: %v", sseEventTypes(eventsA))
	}
	if got := sseCount(eventsA, "message_stop"); got != 1 {
		t.Errorf("message_stop = %d, want 1", got)
	}

	eventsR := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	if got := sseCount(eventsR, "response.completed"); got != 1 {
		t.Errorf("response.completed = %d, want 1", got)
	}
}

// opencodex: EOF with finish_reason content_filter bridges as
// response.incomplete with incomplete_details.reason=content_filter and NO
// response.completed. Parity (added in the stop-semantics gap work).
func TestDifferential_ChatContentFilter(t *testing.T) {
	events := drainSSE(t, newOpenAIToResponsesSSE(strings.NewReader(readDifferential(t, "chat_content_filter.sse")), "gpt-x"))
	if got := sseCount(events, "response.completed"); got != 0 {
		t.Errorf("response.completed = %d, want 0 (content_filter is incomplete)", got)
	}
	inc := sseFilter(events, "response.incomplete")
	if len(inc) != 1 {
		t.Fatalf("response.incomplete = %d, want 1: %v", len(inc), sseEventTypes(events))
	}
	resp := asMap(sseDataMap(t, inc[0])["response"])
	if got := strOf(asMap(resp["incomplete_details"])["reason"]); got != "content_filter" {
		t.Errorf("incomplete_details.reason = %q, want content_filter", got)
	}
}
