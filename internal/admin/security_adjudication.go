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
// the persisted block re-admits the session's requests immediately.
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
