package takeover

import "testing"

func TestDisplayName(t *testing.T) {
	if got := DisplayName("glm-5.2"); got != "glm-5.2" {
		t.Errorf("DisplayName(glm-5.2)=%q want glm-5.2", got)
	}
}
