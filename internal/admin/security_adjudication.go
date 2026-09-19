// security_adjudication.go — the admin surface of the guard AI
// second-opinion channel: the persisted session-block table (list + unblock)
// and the recent-verdict ring. Read methods fail soft (empty results) when
// the channel is not wired; unblock fails closed (error) so a UI button can
// never silently no-op.
package admin

import (
	"errors"
	"net/http"

	"model-proxy/internal/appapi"
)

// SecurityBlocks implements GET /api/security/blocks.
func (s *Service) SecurityBlocks() []appapi.SecurityBlock {
	if s.ports.AdjudicationBlocks == nil {
		return []appapi.SecurityBlock{}
	}
	blocks := s.ports.AdjudicationBlocks()
	if blocks == nil {
		return []appapi.SecurityBlock{}
	}
	return blocks
}

// SecurityUnblock implements DELETE /api/security/blocks/<session>: removing
// the persisted block re-admits the session's requests immediately. The
// removal cascades the operator's risk judgment to the content behind the
// verdict — the same hit bytes are neither re-intercepted nor re-judged.
func (s *Service) SecurityUnblock(sessionID string) error {
	if s.ports.AdjudicationUnblock == nil {
		return errors.New("guard adjudication is not wired")
	}
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if !s.ports.AdjudicationUnblock(sessionID) {
		return &appapi.HTTPError{Status: http.StatusNotFound, Message: "session " + sessionID + " is not blocked"}
	}
	return nil
}

// SecurityAllowed implements GET /api/security/allowed: the operator content
// overrides (hash-keyed; never the bytes themselves).
func (s *Service) SecurityAllowed() []appapi.SecurityAllowed {
	if s.ports.AdjudicationAllowed == nil {
		return []appapi.SecurityAllowed{}
	}
	if allowed := s.ports.AdjudicationAllowed(); allowed != nil {
		return allowed
	}
	return []appapi.SecurityAllowed{}
}

// SecurityDisallow implements DELETE /api/security/allowed/<hash>: revoking
// one override returns the content to fresh adjudication on its next
// occurrence. Fails closed so a UI button can never silently no-op.
func (s *Service) SecurityDisallow(hash string) error {
	if s.ports.AdjudicationDisallow == nil {
		return errors.New("guard adjudication is not wired")
	}
	if hash == "" {
		return errors.New("content hash is required")
	}
	if !s.ports.AdjudicationDisallow(hash) {
		return &appapi.HTTPError{Status: http.StatusNotFound, Message: "content hash is not allowed"}
	}
	return nil
}

// SecurityAdjudications implements GET /api/security/adjudications: the
// recent-verdict ring plus the channel's LLM usage stats and the channel's
// current on/off switch (the leaderboard's noise-reduction hint keys off it).
func (s *Service) SecurityAdjudications() appapi.SecurityAdjudicationFeed {
	if s.ports.AdjudicationRecent == nil {
		return appapi.SecurityAdjudicationFeed{Adjudications: []appapi.SecurityAdjudication{}}
	}
	recent := s.ports.AdjudicationRecent()
	if recent == nil {
		recent = []appapi.SecurityAdjudication{}
	}
	stats := appapi.SecurityAdjudicationStats{}
	if s.ports.AdjudicationStats != nil {
		stats = s.ports.AdjudicationStats()
	}
	enabled := s.ports.AdjudicationEnabled != nil && s.ports.AdjudicationEnabled()
	return appapi.SecurityAdjudicationFeed{Adjudications: recent, Stats: stats, Enabled: enabled}
}
