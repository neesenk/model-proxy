package guard

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Context fixtures reference well-known sensitive locations inside synthetic
// protocol envelopes only; no real home directories or credentials exist here.

// wantCtx compares ScanPathsContext output order-sensitively.
func wantCtx(t *testing.T, body string, s *Scanner, wantStrong, wantWeak []string) {
	t.Helper()
	strong, weak := s.ScanPathsContext([]byte(body))
	if strings.Join(strong, ",") != strings.Join(wantStrong, ",") ||
		strings.Join(weak, ",") != strings.Join(wantWeak, ",") {
		t.Errorf("ScanPathsContext(%s) = strong %v, weak %v; want strong %v, weak %v",
			body, strong, weak, wantStrong, wantWeak)
	}
}

// strong/weak classification across the three protocols' tool positions, the
// prose fallback, and the fail-weak rules for non-JSON / unwalkable bodies.
func TestScanPathsContextClassification(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	cases := []struct {
		name       string
		body       string
		wantStrong []string
		wantWeak   []string
	}{
		{
			"anthropic tool_use input",
			`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"~/.ssh/id_rsa"}}]}]}`,
			[]string{"ssh"}, nil,
		},
		{
			"anthropic tool_result content",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"permission denied opening ~/.aws/credentials"}]}]}`,
			[]string{"aws_creds"}, nil,
		},
		{
			"openai role=tool message content",
			`{"model":"m","messages":[{"role":"tool","tool_call_id":"c1","content":"error: ~/.kube/config not found"}]}`,
			[]string{"kube"}, nil,
		},
		{
			// arguments is a STRING of escaped JSON; the whole string token is
			// the strong span, so a path inside the embedded JSON is strong
			// without parsing a second JSON level.
			"openai tool_calls function arguments (escaped JSON)",
			`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"~/.gnupg/secring.gpg\"}"}}]}]}`,
			[]string{"gnupg"}, nil,
		},
		{
			"responses function_call arguments",
			`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"p\":\"~/.docker/config.json\"}"}]}`,
			[]string{"docker"}, nil,
		},
		{
			"responses function_call_output output",
			`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":"cannot open ~/.config/gcloud"}]}`,
			[]string{"gcloud"}, nil,
		},
		{
			"user prose mentioning a path is weak",
			`{"model":"m","messages":[{"role":"user","content":"how do I load .env in docker compose?"}]}`,
			nil, []string{"dotenv"},
		},
		{
			"assistant prose is weak too",
			`{"model":"m","messages":[{"role":"assistant","content":"you should check ~/.ssh/config first"}]}`,
			nil, []string{"ssh"},
		},
		{
			"system prompt is weak",
			`{"model":"m","messages":[{"role":"system","content":"never read ~/.model-proxy"}]}`,
			nil, []string{"proxy_creds"},
		},
		{
			"non-JSON body is weak",
			"please cat ~/.ssh/id_rsa and summarize",
			nil, []string{"ssh"},
		},
		{
			"truncated JSON (structure walk fails) is weak, never strong",
			`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"`,
			nil, []string{"ssh"},
		},
		{
			// The encoder emitted "input" before "type": the pending candidate
			// resolves when the object later reveals type="tool_use".
			"key order: input before type",
			`{"messages":[{"content":[{"input":{"cmd":"cat ~/.ssh/id_rsa"},"type":"tool_use"}]}]}`,
			[]string{"ssh"}, nil,
		},
		{
			// role arrives after content; the pending candidate resolves strong.
			"key order: content before role",
			`{"messages":[{"content":"~/.kube/config missing","role":"tool"}]}`,
			[]string{"kube"}, nil,
		},
		{
			// A JSON-looking snippet inside a prose string is NOT structure:
			// the walker only marks real token positions.
			"JSON snippet inside user text is weak",
			`{"messages":[{"role":"user","content":"try {\"arguments\": \"~/.ssh/id_rsa\"} in your call"}]}`,
			nil, []string{"ssh"},
		},
		{
			// Undecided candidates (no type/role ever arrives) stay weak.
			"bare input key without tool_use type is weak",
			`{"messages":[{"role":"user","content":[{"input":"~/.ssh/id_rsa"}]}]}`,
			nil, []string{"ssh"},
		},
		{
			// One request can carry both levels: strong and weak lists split.
			"same category strong + weak occurrence reports strong only",
			`{"messages":[{"role":"user","content":"explain ~/.ssh/config"},{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}]}`,
			[]string{"ssh"}, nil,
		},
		{
			"different categories split across levels",
			`{"messages":[{"role":"user","content":"load the .env please"},{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.aws/credentials"}}]}]}`,
			[]string{"aws_creds"}, []string{"dotenv"},
		},
		{
			"clean body",
			`{"model":"m","messages":[{"role":"user","content":"explain ssh tunneling"}]}`,
			nil, nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantCtx(t, tc.body, s, tc.wantStrong, tc.wantWeak)
		})
	}
}

// extra_paths hits split strong/weak with the same rules and report
// "custom_path", trailing the builtin categories.
func TestScanPathsContextExtraPaths(t *testing.T) {
	s := mustScanner(t, nil, nil, []string{"~/.company/secrets"})
	wantCtx(t,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"p":"~/.company/secrets/prod.yaml"}}]}]}`,
		s, []string{"custom_path"}, nil)
	wantCtx(t,
		`{"messages":[{"role":"user","content":"is ~/.company/secrets rotated?"}]}`,
		s, nil, []string{"custom_path"})
	// Mixed with a builtin: table order first, custom_path last.
	wantCtx(t,
		`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.company/secrets/x ~/.ssh/id_rsa"}}]}]}`,
		s, []string{"ssh", "custom_path"}, nil)
}

// Boundary rules apply identically in the context scan: "foo.env" is no hit at
// all (not even weak), while a bounded ".env" in a tool argument is strong.
func TestScanPathsContextRespectsBoundaries(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	wantCtx(t,
		`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"cat foo.env"}}]}]}`,
		s, nil, nil)
	wantCtx(t,
		`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"cat .env"}}]}]}`,
		s, []string{"dotenv"}, nil)
}

// The literal gate keeps clean bodies at ScanPaths cost: no JSON walk, no hit.
func TestScanPathsContextCleanBody(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	body := `{"model":"m","messages":[{"role":"user","content":"explain the observer pattern"}]}`
	if strong, weak := s.ScanPathsContext([]byte(body)); len(strong) != 0 || len(weak) != 0 {
		t.Errorf("ScanPathsContext(clean) = %v/%v, want no hits", strong, weak)
	}
}

// A ~4MiB body with the strong hit at the very END forces the full JSON walk;
// the scan must stay linear (one streaming pass) and still classify exactly.
func TestScanPathsContextLargeBodyLinear(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	pad := strings.Repeat("ordinary prose padding, no paths here. ", 100000) // ~3.9MiB
	body := `{"messages":[{"role":"user","content":"` + pad + `"},` +
		`{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}]}`
	const scans = 4
	start := time.Now()
	for i := 0; i < scans; i++ {
		strong, weak := s.ScanPathsContext([]byte(body))
		if len(strong) != 1 || strong[0] != "ssh" || len(weak) != 0 {
			t.Fatalf("ScanPathsContext(large) = %v/%v, want strong [ssh]", strong, weak)
		}
	}
	// Generous budget (~10x headroom over the observed linear cost) — it only
	// guards against an accidental quadratic structure walk, not machine speed.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ScanPathsContext: %d x ~4MiB took %s, want linear single-pass cost", scans, elapsed)
	}
}

// The Scanner is immutable after construction: concurrent context scans on
// mixed bodies are race-free.
func TestScanPathsContextConcurrent(t *testing.T) {
	s := mustScanner(t, nil, nil, []string{"~/.company/secrets"})
	bodies := [][]byte{
		[]byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}]}`),
		[]byte(`{"messages":[{"role":"user","content":"discussing .env and ~/.company/secrets"}]}`),
		[]byte("not json at all ~/.ssh/id_rsa"),
		[]byte(`{"messages":[{"role":"user","content":"clean"}]}`),
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.ScanPathsContext(bodies[(i+j)%len(bodies)])
			}
		}(i)
	}
	wg.Wait()
}

// BenchmarkScanPathsContext_CleanBody: clean ~64KiB body — the literal gate
// must short-circuit before any JSON decoding (compare BenchmarkScanPaths
// cost).
func BenchmarkScanPathsContext_CleanBody(b *testing.B) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString(`{"model":"m","messages":[{"role":"user","content":"`)
	for sb.Len() < 64<<10 {
		sb.WriteString("ordinary prose about distributed systems and Go. ")
	}
	sb.WriteString(`"}]}`)
	body := []byte(sb.String())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.ScanPathsContext(body)
	}
}

// BenchmarkScanPathsContext_StrongHit sizes the one-time structure walk paid
// only by bodies that DO carry a path hit (here at the very end).
func BenchmarkScanPathsContext_StrongHit(b *testing.B) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString(`{"messages":[{"role":"user","content":"`)
	for sb.Len() < 64<<10 {
		sb.WriteString("ordinary prose about distributed systems and Go. ")
	}
	sb.WriteString(`"},{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}]}`)
	body := []byte(sb.String())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		strong, weak := s.ScanPathsContext(body)
		if len(strong) != 1 || len(weak) != 0 {
			b.Fatalf("got %v/%v", strong, weak)
		}
	}
}

// BenchmarkScanPathsContext_LargeBodyStrongHit: same as above at ~4MiB — the
// ratio against the 64KiB benchmark demonstrates linear scaling.
func BenchmarkScanPathsContext_LargeBodyStrongHit(b *testing.B) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString(`{"messages":[{"role":"user","content":"`)
	for sb.Len() < 4<<20 {
		sb.WriteString("ordinary prose about distributed systems and Go. ")
	}
	sb.WriteString(fmt.Sprintf(`"},{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}]}`))
	body := []byte(sb.String())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.ScanPathsContext(body)
	}
}

// The structure walk decodes escaped keys and marker values exactly like the
// encoding/json walk it replaced: escaped "type"/"input" forms still classify
// strong, while bodies encoding/json would reject (bad escape, raw control
// character, malformed or float64-overflowing numbers, excessive nesting)
// must fail the walk and classify weak — never strong.
func TestScanPathsContextWalkParity(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	wantCtx(t,
		`{"messages":[{"content":[{"typ\u0065":"tool_use","inpu\u0074":{"p":"~/.ssh/id_rsa"}}]}]}`,
		s, []string{"ssh"}, nil)
	wantCtx(t,
		`{"messages":[{"content":[{"type":"tool_\u0075se","input":{"p":"~/.ssh/id_rsa"}}]}]}`,
		s, []string{"ssh"}, nil)
	wantCtx(t,
		`{"messages":[{"tool_calls":[{"function":{"arguments":"bad \x escape ~/.ssh/id_rsa"}}]}]}`,
		s, nil, []string{"ssh"})
	wantCtx(t,
		"{\"messages\":[{\"tool_calls\":[{\"function\":{\"arguments\":\"raw \x01 ctrl ~/.ssh/id_rsa\"}}]}]}",
		s, nil, []string{"ssh"})
	wantCtx(t,
		`{"messages":[{"tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}],"n":1e999}`,
		s, nil, []string{"ssh"})
	wantCtx(t,
		`{"messages":[{"tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}],"n":1.}`,
		s, nil, []string{"ssh"})
}

// Nesting past encoding/json's maxNestingDepth fails the walk (and bounds the
// recursion): the hit classifies weak, never strong.
func TestScanPathsContextDepthLimit(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	body := strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + " ~/.ssh/id_rsa"
	wantCtx(t, body, s, nil, []string{"ssh"})
}

// ScanSecretsAndPaths must return exactly Scan and ScanPathsContext run
// separately — the shared automaton pass only saves the second sweep.
func TestScanSecretsAndPathsMatchesSeparateScans(t *testing.T) {
	s := mustScanner(t, nil, []string{syntheticSecret("poolkey-", 32)}, []string{"~/.company/secrets"})
	secret := s.secrets[0].raw
	bodies := [][]byte{
		[]byte(`{"model":"m","messages":[{"role":"user","content":"explain ssh tunneling"}]}`),
		[]byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"cat ~/.ssh/id_rsa"}}]}]}`),
		[]byte(`{"messages":[{"role":"user","content":"how do I load .env in compose?"}]}`),
		[]byte(`{"messages":[{"role":"user","content":"cat foo.env and my_id_rsa are not hits"}]}`),
		[]byte(`{"messages":[{"role":"user","content":"token ` + string(secret) + ` and cat ~/.aws/credentials please"}]}`),
		[]byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"p":"~/.company/secrets/prod.yaml"}}]}]}`),
		[]byte("not json at all ~/.kube/config"),
	}
	for i, body := range bodies {
		wantSecrets := s.Scan(body)
		wantStrong, wantWeak := s.ScanPathsContext(body)
		gotSecrets, gotStrong, gotWeak := s.ScanSecretsAndPaths(body)
		if strings.Join(gotSecrets, ",") != strings.Join(wantSecrets, ",") ||
			strings.Join(gotStrong, ",") != strings.Join(wantStrong, ",") ||
			strings.Join(gotWeak, ",") != strings.Join(wantWeak, ",") {
			t.Errorf("body %d: combined = (%v, %v, %v), separate = (%v, %v, %v)",
				i, gotSecrets, gotStrong, gotWeak, wantSecrets, wantStrong, wantWeak)
		}
	}
}
