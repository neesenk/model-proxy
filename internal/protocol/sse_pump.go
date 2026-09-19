// sse_pump.go owns the SSE frame machinery shared by the six streaming
// converters: line scanning, optional one-shot leading-BOM tolerance,
// multi-data-line folding, event: line classification, and blank-line
// dispatch (including the trailing frame synthesized at scanner exhaustion).
// Converter-specific terminal emission and payload dispatch stay in the
// converters behind sseFrameHooks; the pump owns only frame boundaries.
package protocol

import (
	"bufio"
	"strings"
)

// sseFrameHooks is the converter side of the shared SSE pump. Implementations
// are the stream converters themselves; the pump calls back at each decision
// point. Every method may append to the converter's output buffer.
type sseFrameHooks interface {
	// hasOutput reports whether the converter has buffered output to drain
	// (the pump's loop condition).
	hasOutput() bool
	// isDone reports whether the converter reached a terminal state.
	isDone() bool
	// drainDone runs the done-branch terminal emission. It returns true when
	// no output remains and the pump's caller must return io.EOF.
	drainDone() (eof bool)
	// scanError handles a scanner error (line too long, ...): warn + the
	// converter's terminated-unexpectedly emission.
	scanError(err error)
	// streamEnd handles clean scanner exhaustion with no frame open: the
	// stopRsn/finish shortcut or the premature-terminal emission.
	streamEnd()
	// dispatch consumes one dispatched frame: frameEvent is the frame's
	// event: name, dataEvents the per-data-line event names (nil unless the
	// converter tracks folded frames), payload the folded data. "[DONE]"
	// handling belongs to the converter.
	dispatch(frameEvent string, dataEvents []string, payload string)
}

// pumpSSEFrames drives the shared pump loop until the converter has output to
// drain or the stream ended. It returns true only when the converter's
// drainDone reported an empty buffer (the caller then returns io.EOF).
//
// bomStripped, when non-nil, enables the one leading UTF-8 BOM tolerance
// (some gateways prepend it); the flag persists on the converter across Read
// calls so a mid-stream BOM stays untouched. trackEvents collects the
// per-data-line event names for folded multi-frame payloads (the
// responses-direction converters).
func pumpSSEFrames(h sseFrameHooks, sc *bufio.Scanner, bomStripped *bool, trackEvents bool) (eof bool) {
	pendingEvent := ""
	pendData := ""    // folded data lines of the SSE frame in progress
	pendOpen := false // a data: line opened the current frame (an empty one folds to "")
	var pendEvents []string
	for !h.hasOutput() {
		if h.isDone() {
			if h.drainDone() {
				return true
			}
			return false
		}
		line := ""
		if sc.Scan() {
			// Keep prefix matching strict: only strip the trailing CR from
			// CRLF. Leading whitespace would turn " data:" into a data line,
			// which is not spec-compliant and hides malformed upstream bytes.
			line = strings.TrimRight(sc.Text(), "\r")
			if bomStripped != nil && !*bomStripped {
				// Tolerate one leading UTF-8 BOM at stream start (some gateways
				// prepend it); mid-stream BOMs stay untouched.
				*bomStripped = true
				line = strings.TrimPrefix(line, "\ufeff")
			}
		} else if pendOpen {
			// Scanner exhausted with a frame in progress: synthesize the
			// dispatch blank line (the SSE spec delivers a trailing frame
			// without its final blank line). The next iteration takes the
			// normal exhaustion path with no frame open.
			line = ""
		} else {
			if err := sc.Err(); err != nil {
				h.scanError(err)
				continue
			}
			h.streamEnd()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			pendData = appendSSEData(pendData, pendOpen, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if trackEvents {
				pendEvents = append(pendEvents, pendingEvent)
			}
			pendOpen = true
			continue
		}
		// Classify the frame-terminating line first — it may open the NEXT
		// frame's event type; the closing frame keeps its own.
		frameEvent := pendingEvent
		if line == "" {
			pendingEvent = ""
		} else if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if line != "" || !pendOpen {
			// Only a blank line dispatches a frame (SSE spec): event:/retry:/
			// comment lines belong to the frame in progress even when they
			// trail its data lines — dispatching on them would classify the
			// frame under the previous event and leak the real one forward.
			continue
		}
		payload := pendData
		pendData, pendOpen = "", false
		var dataEvents []string
		if trackEvents {
			dataEvents = pendEvents
			pendEvents = nil
		}
		h.dispatch(frameEvent, dataEvents, payload)
	}
	return false
}
