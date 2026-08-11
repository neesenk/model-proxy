package fusion

import (
	"context"
	"encoding/json"
	"time"

	"model-proxy/internal/config"
)

// ObservationFromResult projects a panel or judge result to a retained value.
func ObservationFromResult(result LegResult, kind string) LegObservation {
	observation := LegObservation{
		Provider:  result.Provider,
		Model:     result.Model,
		Kind:      kind,
		Status:    result.Status,
		LatencyMs: result.LatencyMs,
		Input:     result.Usage.Input,
		Output:    result.Usage.Output,
	}
	if result.Err != nil {
		observation.Err = result.Err.Error()
	}
	return observation
}

// ReconcileLegs keeps every configured panel member visible. Results not
// received by collection are marked as cut by quorum/grace cancellation.
func ReconcileLegs(panel []config.RouteTarget, received []LegResult) []LegObservation {
	byIndex := make(map[int]LegResult, len(received))
	for _, result := range received {
		byIndex[result.Index] = result
	}
	legs := make([]LegObservation, 0, len(panel))
	for index, member := range panel {
		if result, ok := byIndex[index]; ok {
			legs = append(legs, ObservationFromResult(result, "panel"))
			continue
		}
		legs = append(legs, LegObservation{
			Provider: member.Provider,
			Model:    member.Model,
			Kind:     "panel",
			Cut:      true,
			Err:      "cut by quorum/grace",
		})
	}
	return legs
}

// CollectResults gathers leg results until quorum is unreachable, all legs
// finish, or a single grace window expires after quorum is first met. Results
// must be buffered by the caller so sends after cancellation cannot block.
func CollectResults(
	results <-chan LegResult,
	launched int,
	quorum int,
	graceDuration time.Duration,
	cancel context.CancelFunc,
) (successes, received []LegResult) {
	if launched <= 0 || quorum <= 0 {
		if quorum <= 0 {
			return nil, nil
		}
		if cancel != nil {
			cancel()
		}
		return nil, nil
	}
	var timer *time.Timer
	var grace <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	inFlight := launched
	for inFlight > 0 {
		if len(successes)+inFlight < quorum {
			if cancel != nil {
				cancel()
			}
			return nil, received
		}
		select {
		case result, ok := <-results:
			if !ok {
				if len(successes) < quorum {
					return nil, received
				}
				return successes, received
			}
			inFlight--
			received = append(received, result)
			if result.Err == nil {
				successes = append(successes, result)
			}
			if len(successes) >= quorum && timer == nil && inFlight > 0 {
				timer = time.NewTimer(graceDuration)
				grace = timer.C
			}
		case <-grace:
			if cancel != nil {
				cancel()
			}
			return successes, received
		}
	}
	if len(successes) < quorum {
		return nil, received
	}
	return successes, received
}

// HasAssistantTurn reports whether a message/input array contains an assistant
// turn. Invalid JSON is intentionally treated as no known assistant turn.
func HasAssistantTurn(body []byte) bool {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	for _, key := range []string{"messages", "input"} {
		messages, ok := request[key].([]any)
		if !ok {
			continue
		}
		for _, message := range messages {
			if item, ok := message.(map[string]any); ok && item["role"] == "assistant" {
				return true
			}
		}
	}
	return false
}
