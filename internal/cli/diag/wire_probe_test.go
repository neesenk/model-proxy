package diag

import (
	"testing"

	configdomain "model-proxy/internal/config"
)

// TestWireProbeModelLocal: model pick order is provider's first configured
// model → first route target → first derived route target → "".
func TestWireProbeModelLocal(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"with-models": {Provider: "static", Models: []string{"m1", "m2"}},
			"via-route":   {Provider: "static"},
			"via-derived": {Provider: "static"},
			"none":        {Provider: "static"},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"r": {{Provider: "via-route", Model: "route-model"}},
		},
	}
	derived := map[string][]configdomain.RouteTarget{
		"d": {{Provider: "via-derived", Model: "derived-model"}},
	}
	for _, tc := range []struct {
		prov, want string
	}{
		{"with-models", "m1"},
		{"via-route", "route-model"},
		{"via-derived", "derived-model"},
		{"none", ""},
	} {
		if got := wireProbeModelLocal(cfg, derived, tc.prov); got != tc.want {
			t.Errorf("wireProbeModelLocal(%s) = %q, want %q", tc.prov, got, tc.want)
		}
	}
}
