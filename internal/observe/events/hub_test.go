package events

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHubPublishSubscribeRecent(t *testing.T) {
	h := NewHub()
	h.Publish(Event{Type: "end", Agent: "claude-code", Exposed: "glm", Status: 200})

	ch, recent, cancel := h.Subscribe()
	if len(recent) != 1 || recent[0].Agent != "claude-code" {
		t.Errorf("recent replay = %+v, want one claude-code event", recent)
	}
	recent[0].Agent = "mutated"
	if got := h.Snapshot()[0].Agent; got != "claude-code" {
		t.Errorf("Subscribe recent snapshot mutated Hub storage: Agent = %q", got)
	}
	h.Publish(Event{Type: "end", Agent: "codex", Status: 500})
	select {
	case event := <-ch:
		if event.Agent != "codex" || event.Status != 500 {
			t.Errorf("event = %+v, want codex/500", event)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive published event")
	}

	cancel()
	cancel() // cancellation is idempotent
	h.Publish(Event{Type: "end", RequestID: "after-cancel"})
	select {
	case event := <-ch:
		t.Errorf("received event after cancel: %+v", event)
	default:
	}
}

func TestHubDropsSlowSubscriber(t *testing.T) {
	h := NewHub()
	ch, _, cancel := h.Subscribe()
	defer cancel()
	for i := 0; i < 32; i++ {
		h.Publish(Event{Type: "end", Status: i})
	}
	done := make(chan struct{})
	go func() {
		h.Publish(Event{Type: "end", RequestID: "overflow"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
	for want := 0; want < 32; want++ {
		select {
		case event := <-ch:
			if event.RequestID == "overflow" || event.Status != want {
				t.Fatalf("buffered event %d = %+v, want original status %d", want, event, want)
			}
		default:
			t.Fatalf("received only %d buffered events, want 32", want)
		}
	}
	select {
	case event := <-ch:
		t.Fatalf("received overflow event instead of dropping it: %+v", event)
	default:
	}
}

func TestHubRecentRingAndFindEnd(t *testing.T) {
	h := NewHub()
	for i := 0; i < recentModelCap+5; i++ {
		h.Publish(Event{Type: "start", RequestID: "other"})
	}
	h.Publish(Event{Type: "end", RequestID: "wanted", Status: 200})
	h.Publish(Event{Type: "end", RequestID: "wanted", Status: 201})

	snapshot := h.Snapshot()
	if len(snapshot) != recentModelCap {
		t.Fatalf("Snapshot length = %d, want %d", len(snapshot), recentModelCap)
	}
	snapshot[0].Type = "mutated"
	if h.Snapshot()[0].Type == "mutated" {
		t.Fatal("Snapshot returned mutable internal storage")
	}
	event, ok := h.FindEnd("wanted")
	if !ok || event.Status != 201 {
		t.Fatalf("FindEnd = %+v, %v; want newest status 201", event, ok)
	}
	if _, ok := h.FindEnd("missing"); ok {
		t.Fatal("FindEnd reported a missing request")
	}
}

func TestHubPerStreamRingsModelFloodKeepsMCP(t *testing.T) {
	h := NewHub()
	h.Publish(Event{Type: "end", RequestID: "mcp-old", Protocol: "mcp", Status: 200})
	for i := 0; i < recentModelCap+50; i++ {
		h.Publish(Event{Type: "progress", RequestID: "model", Protocol: "anthropic", Ts: int64(i)})
	}
	h.Publish(Event{Type: "end", RequestID: "mcp-new", Protocol: "mcp", Status: 200, Ts: recentModelCap + 100})

	_, replay, cancel := h.Subscribe()
	defer cancel()
	var mcpSeen []string
	for _, e := range replay {
		if e.Protocol == "mcp" {
			mcpSeen = append(mcpSeen, e.RequestID)
		}
	}
	if len(mcpSeen) != 2 || mcpSeen[0] != "mcp-old" || mcpSeen[1] != "mcp-new" {
		t.Fatalf("mcp events after model flood = %v, want both retained", mcpSeen)
	}
	if len(replay) != recentModelCap+2 {
		t.Fatalf("merged replay length = %d, want %d (model ring capped, mcp ring kept)", len(replay), recentModelCap+2)
	}
	for i := 1; i < len(replay); i++ {
		if replay[i-1].Ts > replay[i].Ts {
			t.Fatalf("merged replay not ts-ascending at %d: %d > %d", i, replay[i-1].Ts, replay[i].Ts)
		}
	}
}

func TestHubMCPFloodKeepsModel(t *testing.T) {
	h := NewHub()
	h.Publish(Event{Type: "end", RequestID: "model-old", Protocol: "anthropic", Status: 200})
	for i := 0; i < recentMCPCap+50; i++ {
		h.Publish(Event{Type: "end", RequestID: "mcp", Protocol: "mcp", Ts: int64(i)})
	}
	snapshot := h.Snapshot()
	found := false
	for _, e := range snapshot {
		if e.RequestID == "model-old" {
			found = true
		}
	}
	if !found {
		t.Fatal("model event evicted by mcp flood; rings must be independent")
	}
	mcpRows := 0
	for _, e := range snapshot {
		if e.Protocol == "mcp" && e.RequestID == "mcp" {
			mcpRows++
		}
	}
	if mcpRows != recentMCPCap {
		t.Fatalf("mcp ring size = %d, want %d (its own cap)", mcpRows, recentMCPCap)
	}
}

func TestHubRingTruncatesRetainedTextOnly(t *testing.T) {
	h := NewHub()
	ch, _, cancel := h.Subscribe()
	defer cancel()
	// Multibyte padding forces the cut point to land inside a rune sequence.
	full := strings.Repeat("世", ringTextCap) + "tail"
	h.Publish(Event{Type: "progress", RequestID: "p", Protocol: "anthropic", Text: full})

	select {
	case e := <-ch:
		if e.Text != full {
			t.Fatalf("live subscriber text truncated to %d bytes; delivery must carry the full prefix", len(e.Text))
		}
	default:
		t.Fatal("subscriber did not receive the published event")
	}
	snapshot := h.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(snapshot))
	}
	retained := snapshot[0].Text
	if len(retained) > ringTextCap {
		t.Fatalf("retained text = %d bytes, want ≤ %d", len(retained), ringTextCap)
	}
	if retained != full[:len(retained)] {
		t.Fatal("retained text is not a prefix of the original")
	}
	if !utf8.ValidString(retained) {
		t.Fatal("retained text cut mid-rune")
	}
	if n := utf8.RuneCountInString(retained); n != ringTextCap/3 {
		t.Fatalf("retained rune count = %d, want %d (cut on rune boundary at the cap)", n, ringTextCap/3)
	}
}

func TestHubFindEndAcrossStreams(t *testing.T) {
	h := NewHub()
	h.Publish(Event{Type: "end", RequestID: "m1", Protocol: "anthropic", Status: 200, Ts: 1})
	h.Publish(Event{Type: "end", RequestID: "x1", Protocol: "mcp", Status: 200, Ts: 2})
	h.Publish(Event{Type: "end", RequestID: "m1", Status: 201, Ts: 3})
	h.Publish(Event{Type: "end", RequestID: "x1", Protocol: "mcp", Status: 202, Ts: 4})
	if e, ok := h.FindEnd("m1"); !ok || e.Status != 201 {
		t.Fatalf("FindEnd(model id) = %+v, %v; want status 201", e, ok)
	}
	if e, ok := h.FindEnd("x1"); !ok || e.Status != 202 {
		t.Fatalf("FindEnd(mcp id) = %+v, %v; want status 202", e, ok)
	}
}

func TestNilHubPublishAndFindEnd(t *testing.T) {
	var h *Hub
	h.Publish(Event{Type: "end"})
	if snapshot := h.Snapshot(); snapshot != nil {
		t.Fatalf("nil Hub Snapshot = %+v, want nil", snapshot)
	}
	if _, ok := h.FindEnd("missing"); ok {
		t.Fatal("nil Hub found an event")
	}
}

func TestHubHasSubscribers(t *testing.T) {
	h := NewHub()
	if h.HasSubscribers() {
		t.Fatal("new hub should have no subscribers")
	}
	_, _, cancel := h.Subscribe()
	if !h.HasSubscribers() {
		t.Fatal("hub should report subscribers after Subscribe")
	}
	cancel()
	if h.HasSubscribers() {
		t.Fatal("hub should report no subscribers after cancel")
	}
	var nilHub *Hub
	if nilHub.HasSubscribers() {
		t.Fatal("nil hub should report no subscribers")
	}
}
