package guard

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var ruleNameRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func jsonUnmarshalRules(f *rulesFile) error {
	return json.Unmarshal(rulesJSON, f)
}

// Synthetic fixture material only — no real credentials anywhere in tests
// (AGENTS.md credential red line). High-entropy payloads come from a
// deterministic splitmix64 PRNG so upstream entropy thresholds are met
// reproducibly.
type fixtureRNG struct{ state uint64 }

func newFixtureRNG(seed uint64) *fixtureRNG { return &fixtureRNG{state: seed} }

func (r *fixtureRNG) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *fixtureRNG) chars(n int, alphabet string) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.next()%uint64(len(alphabet))]
	}
	return string(b)
}

const (
	alphaAlnum   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alphaLower36 = "abcdefghijklmnopqrstuvwxyz0123456789"
	alphaHex     = "0123456789abcdef"
	alphaB64     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	alphaWord    = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
	alphaUpper32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	alphaBech32  = "QPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L"
	alphaDigits  = "0123456789"
)

// ruleFixture pairs one positive synthetic vector (must report exactly the
// rule name through Scan — a broken prefilter literal fails loudly here) with
// one negative vector (same prefix, truncated/wrong shape).
type ruleFixture struct {
	name     string
	positive string
	negative string
}

// ruleFixtures returns one fixture per embedded rule. Positives embed the
// synthetic secret in a realistic assignment context.
func ruleFixtures() []ruleFixture {
	rng := newFixtureRNG(0x5eed)
	c := func(n int, alphabet string) string { return rng.chars(n, alphabet) }
	kv := func(key, value string) string { return key + ` = "` + value + `"` }

	return []ruleFixture{
		// --- model-proxy hand-written rules (migrated from the old table) ---
		{"anthropic_api_key", kv("anthropic_key", "sk-ant-api03-"+c(40, alphaWord)), "sk-ant-" + c(10, alphaWord)},
		{"openai_api_key", kv("openai_key", "sk-"+c(30, alphaWord)), "sk-" + c(10, alphaWord)},
		{"github_token", kv("gh", "ghp_"+c(36, alphaAlnum)), "ghp_" + c(20, alphaAlnum)},
		{"github_fine_grained_pat", kv("ghp", "github_pat_"+c(30, alphaAlnum+"_")), "github_pat_" + c(10, alphaWord)},
		{"aws_access_key_id", kv("aws_access_key_id", "AKIA"+c(16, alphaUpper32)), "AKIA" + c(10, alphaUpper32)},
		{"google_api_key", kv("key", "AIza"+c(35, alphaWord)), "AIza" + c(10, alphaWord)},
		{"pem_private_key", "-----BEGIN RSA PRIVATE KEY-----\n" + c(32, alphaB64) + "\n-----END RSA PRIVATE KEY-----",
			"-----BEGIN PUBLIC KEY-----\n" + c(32, alphaB64) + "\n-----END PUBLIC KEY-----"},

		// --- gitleaks selection ---
		{"stripe_access_token", kv("stripe", "sk_live_"+c(24, alphaAlnum)), "sk_live_" + c(5, alphaAlnum)},
		{"aws_access_token", kv("aws", "ABIA"+c(16, alphaUpper32)), "ABIA" + c(10, alphaUpper32)},
		{"alibaba_access_key_id", kv("alibaba", "LTAI"+c(20, alphaLower36)), "LTAI" + c(10, alphaLower36)},
		{"artifactory_api_key", kv("artifactory", "AKCp"+c(69, alphaAlnum)), "AKCp" + c(20, alphaAlnum)},
		{"databricks_api_token", kv("databricks", "dapi"+c(32, alphaHex)), "dapi" + c(10, alphaHex)},
		{"digitalocean_access_token", kv("do", "doo_v1_"+c(64, alphaHex)), "doo_v1_" + c(20, alphaHex)},
		{"digitalocean_pat", kv("do", "dop_v1_"+c(64, alphaHex)), "dop_v1_" + c(20, alphaHex)},
		{"doppler_api_token", kv("doppler", "dp.pt."+c(43, alphaLower36)), "dp.pt." + c(10, alphaLower36)},
		{"dynatrace_api_token", kv("dynatrace", "dt0c01."+c(24, alphaLower36)+"."+c(64, alphaLower36)), "dt0c01." + c(10, alphaLower36)},
		{"flyio_access_token", kv("fly", "fo1_"+c(43, alphaAlnum)), "fo1_" + c(10, alphaWord)},
		{"gitlab_pat", kv("gitlab", "glpat-"+c(20, alphaWord)), "glpat-" + c(10, alphaWord)},
		{"gitlab_ptt", kv("gitlab", "glptt-"+c(40, alphaHex)), "glptt-" + c(10, alphaHex)},
		{"gitlab_runner_authentication_token", kv("gitlab", "glrt-"+c(20, alphaWord)), "glrt-" + c(10, alphaWord)},
		{"gitlab_oauth_app_secret", kv("gitlab", "gloas-"+c(64, alphaWord)), "gloas-" + c(10, alphaWord)},
		{"grafana_api_key", kv("grafana", "eyJrIjoi"+c(80, alphaAlnum)+"="), "eyJrIjoi" + c(20, alphaAlnum)},
		{"grafana_cloud_api_token", kv("grafana", "glc_"+c(40, alphaB64)), "glc_" + c(10, alphaB64)},
		{"grafana_service_account_token", kv("grafana", "glsa_"+c(32, alphaAlnum)+"_"+c(8, alphaHex)), "glsa_" + c(10, alphaAlnum)},
		{"hashicorp_tf_api_token", kv("tfe", c(14, alphaLower36)+".atlasv1."+c(64, alphaLower36+"-_=")),
			"x.atlasv1." + c(60, alphaLower36)},
		{"huggingface_access_token", kv("hf", "hf_"+c(34, "abcdefghijklmnopqrstuvwxyz")), "hf_" + c(10, "abcdefghijklmnopqrstuvwxyz")},
		{"jwt", kv("token", "eyJ"+c(20, alphaAlnum)+".eyJ"+c(20, alphaWord+"/")+"."+c(12, alphaWord+"/")), "eyJabc.def"},
		{"linear_api_key", kv("linear", "lin_api_"+c(40, alphaLower36)), "lin_api_" + c(10, alphaLower36)},
		{"notion_api_token", kv("notion", "ntn_"+c(11, alphaDigits)+c(35, alphaAlnum)), "ntn_" + c(10, alphaAlnum)},
		{"npm_access_token", kv("npm", "npm_"+c(36, alphaLower36)), "npm_" + c(10, alphaLower36)},
		{"openshift_user_token", kv("oc", "sha256~"+c(43, alphaWord)), "sha256~" + c(10, alphaWord)},
		{"perplexity_api_key", kv("pplx", "pplx-"+c(48, alphaAlnum)), "pplx-" + c(10, alphaAlnum)},
		{"planetscale_api_token", kv("pscale", "pscale_tkn_"+c(40, alphaWord+"=.")), "pscale_tkn_" + c(10, alphaWord)},
		{"postman_api_token", kv("postman", "PMAK-"+c(24, alphaHex)+"-"+c(34, alphaHex)), "PMAK-" + c(10, alphaHex)},
		{"pulumi_api_token", kv("pulumi", "pul-"+c(40, alphaHex)), "pul-" + c(10, alphaHex)},
		{"pypi_upload_token", kv("pypi", "pypi-AgEIcHlwaS5vcmc"+c(60, alphaWord)), "pypi-AgEIcHlwaS5vcmc" + c(10, alphaWord)},
		{"rubygems_api_token", kv("rubygems", "rubygems_"+c(48, alphaHex)), "rubygems_" + c(10, alphaHex)},
		{"sendgrid_api_token", kv("sendgrid", "SG."+c(66, alphaLower36+"=_.")), "SG." + c(10, alphaLower36)},
		{"sendinblue_api_token", kv("sendinblue", "xkeysib-"+c(64, alphaHex)+"-"+c(16, alphaLower36)), "xkeysib-" + c(10, alphaHex)},
		{"sentry_user_token", kv("sentry", "sntryu_"+c(64, alphaHex)), "sntryu_" + c(10, alphaHex)},
		{"shippo_api_token", kv("shippo", "shippo_live_"+c(40, alphaHex)), "shippo_live_" + c(10, alphaHex)},
		{"shopify_access_token", kv("shopify", "shpat_"+c(32, alphaHex)), "shpat_" + c(10, alphaHex)},
		{"slack_app_token", kv("slack", "xapp-1-"+c(12, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")+"-123456789-"+c(16, alphaLower36)), "xapp-1-ABC"},
		{"slack_bot_token", kv("slack", "xoxb-"+c(10, alphaDigits)+"-"+c(10, alphaDigits)+"-"+c(24, alphaAlnum+"-")), "xoxb-123-456"},
		{"slack_config_access_token", kv("slack", "xoxe.xoxb-1-"+c(164, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")), "xoxe.xoxb-1-" + c(10, alphaAlnum)},
		{"slack_user_token", kv("slack", "xoxp-"+c(10, alphaDigits)+"-"+c(10, alphaDigits)+"-"+c(10, alphaDigits)+"-"+c(30, alphaAlnum+"-")), "xoxp-123-456-789-short"},
		{"slack_webhook_url", "url = https://hooks.slack.com/services/" + c(45, alphaB64), "hooks.slack.com/services/short"},
		{"sourcegraph_access_token", kv("sg", "sgp_"+c(16, alphaHex)+"_"+c(40, alphaHex)), "sgp_" + c(10, alphaHex)},
		{"square_access_token", kv("square", "sq0atp-"+c(30, alphaWord)), "sq0atp-" + c(10, alphaWord)},
		{"vault_batch_token", kv("vault", "hvb."+c(140, alphaWord)), "hvb." + c(10, alphaWord)},
		{"vault_service_token", kv("vault", "hvs."+c(95, alphaWord)), "hvs." + c(10, alphaWord)},
		{"age_secret_key", kv("age", "AGE-SECRET-KEY-1"+c(58, alphaBech32)), "AGE-SECRET-KEY-1" + c(10, alphaBech32)},
		{"1password_service_account_token", kv("1password", "ops_eyJ"+c(260, alphaB64)+"=="), "ops_eyJ" + c(20, alphaB64)},
	}
}

// TestEmbeddedRuleFixtures forces every embedded rule through Scan with its
// positive and negative vector. A literal that is not a true substring of the
// rule's matches (the prefilter invariant) turns the positive case red
// immediately.
func TestEmbeddedRuleFixtures(t *testing.T) {
	fixtures := map[string]ruleFixture{}
	for _, fx := range ruleFixtures() {
		if _, dup := fixtures[fx.name]; dup {
			t.Fatalf("duplicate fixture for %s", fx.name)
		}
		fixtures[fx.name] = fx
	}
	for _, r := range embeddedRules {
		fx, ok := fixtures[r.name]
		if !ok {
			t.Errorf("embedded rule %q has no fixture", r.name)
			continue
		}
		delete(fixtures, r.name)
		if got := Scan([]byte(fx.positive)); len(got) != 1 || got[0] != r.name {
			t.Errorf("%s positive: Scan = %v, want exactly [%s]", r.name, got, r.name)
		}
		if got := Scan([]byte(fx.negative)); len(got) != 0 {
			t.Errorf("%s negative: Scan = %v, want no hits", r.name, got)
		}
		if redacted := Redact([]byte(fx.positive)); strings.Contains(string(redacted), fx.positive) {
			t.Errorf("%s positive: Redact left the secret in the body", r.name)
		}
	}
	for name := range fixtures {
		t.Errorf("fixture %q does not correspond to any embedded rule", name)
	}
}

// TestEntropyPostFilter: a regex-shaped but low-entropy hit is discarded for
// rules carrying an upstream entropy threshold.
func TestEntropyPostFilter(t *testing.T) {
	// gitlab_pat has entropy 3; 20 identical chars score 0.
	low := "glpat-" + strings.Repeat("a", 20)
	if got := Scan([]byte(low)); len(got) != 0 {
		t.Errorf("Scan(low-entropy glpat lookalike) = %v, want no hits", got)
	}
	// A rule without an entropy threshold (model-proxy anthropic) still hits
	// on the same low-entropy shape — the filter is per-rule.
	noEntropy := "sk-ant-" + strings.Repeat("a", 25)
	if got := Scan([]byte(noEntropy)); len(got) != 1 || got[0] != "anthropic_api_key" {
		t.Errorf("Scan(low-entropy sk-ant) = %v, want [anthropic_api_key]", got)
	}
}

// TestRulesJSONSchema validates the embedded rules file invariants beyond
// compilation (which package init already enforces fail-closed).
func TestRulesJSONSchema(t *testing.T) {
	var f rulesFile
	if err := jsonUnmarshalRules(&f); err != nil {
		t.Fatalf("rules.json does not parse: %v", err)
	}
	if f.Upstream == "" || f.Attribution == "" {
		t.Errorf("rules.json must carry upstream version and attribution")
	}
	seen := map[string]bool{}
	for _, e := range f.Rules {
		if !ruleNameRE.MatchString(e.Name) {
			t.Errorf("rule name %q: want [a-z0-9_]{1,64}", e.Name)
		}
		if seen[e.Name] {
			t.Errorf("duplicate rule name %q", e.Name)
		}
		seen[e.Name] = true
		if e.Source != "gitleaks" && e.Source != "model-proxy" {
			t.Errorf("rule %s: unknown source %q", e.Name, e.Source)
		}
		if e.SourceRule == "" {
			t.Errorf("rule %s: empty source_rule", e.Name)
		}
		if e.Entropy != nil && *e.Entropy <= 0 {
			t.Errorf("rule %s: entropy must be positive", e.Name)
		}
		for _, lit := range e.Literals {
			if len(lit) < 3 {
				t.Errorf("rule %s: literal %q too short to be a useful prefilter", e.Name, lit)
			}
		}
	}
}

// TestRuleLiteralsAreReachable is the reverse half of the prefilter
// invariant: not only must every rule match contain a literal (enforced by
// the positive fixtures through Scan), every literal must be contained in at
// least one POSSIBLE match of its rule's regex. A literal the regex can never
// produce (e.g. left over from a case-insensitive upstream regex after the
// regex was made case-sensitive, or containing a byte the regex treats as a
// wildcard) is dead weight and signals the forward invariant is broken for
// the shapes it was meant to cover.
//
// Witnesses are derived from the rule's positive fixture: if the literal is
// absent from every fixture match, substitute it for the fixture match's
// literal and re-run the regex; candidates pad the post-literal remainder to
// satisfy alternatives with different length quantifiers (e.g. A3T vs ABIA,
// fm1a_ vs fo1_).
func TestRuleLiteralsAreReachable(t *testing.T) {
	fixtures := map[string]ruleFixture{}
	for _, fx := range ruleFixtures() {
		fixtures[fx.name] = fx
	}
	containsLiteral := func(body string, locs [][]int, lit string) bool {
		for _, loc := range locs {
			if strings.Contains(body[loc[0]:loc[1]], lit) {
				return true
			}
		}
		return false
	}
	for _, r := range embeddedRules {
		fx, ok := fixtures[r.name]
		if !ok {
			continue // missing fixture already reported by TestEmbeddedRuleFixtures
		}
		body := fx.positive
		locs := r.re.FindAllStringIndex(body, -1)
		// Anchor match: the first fixture match containing a literal.
		var anchor []int
		var anchorLit string
		for _, loc := range locs {
			for _, lit := range r.literals {
				if strings.Contains(body[loc[0]:loc[1]], string(lit)) {
					anchor, anchorLit = loc, string(lit)
					break
				}
			}
			if anchor != nil {
				break
			}
		}
		if anchor == nil {
			t.Errorf("rule %s: no fixture-positive match contains any literal", r.name)
			continue
		}
		whole := body[anchor[0]:anchor[1]]
		cut := strings.Index(whole, anchorLit)
		pre, post := whole[:cut], whole[cut+len(anchorLit):]
		// postCore drops the trailing-context byte ([\x60'"\s;] and friends)
		// many upstream regexes capture, so it can be repeated to satisfy
		// longer length quantifiers.
		postCore := strings.TrimRight(post, "`'\" \t\n\r;\\")
		for _, litB := range r.literals {
			lit := string(litB)
			if containsLiteral(body, locs, lit) {
				continue
			}
			candidates := []string{
				body[:anchor[0]] + pre + lit + post + body[anchor[1]:],
				body[:anchor[0]] + pre + lit + strings.Repeat(postCore, 4) + body[anchor[1]:],
			}
			for j := 1; j <= len(postCore); j++ {
				candidates = append(candidates,
					body[:anchor[0]]+pre+lit+post+postCore[:j]+body[anchor[1]:])
			}
			reachable := false
			for _, cand := range candidates {
				if containsLiteral(cand, r.re.FindAllStringIndex(cand, -1), lit) {
					reachable = true
					break
				}
			}
			if !reachable {
				t.Errorf("rule %s: literal %q is not a substring of any reachable regex match", r.name, lit)
			}
		}
	}
}

// TestCaseVariantFixtures pins the case-sensitive alternation shape of the
// rules whose upstream regex was (?i) while the prefilter literals are
// case-sensitive: the enumerated case forms hit, everything else (mixed
// case, wildcard-byte substitutions) is rejected by the regex itself.
func TestCaseVariantFixtures(t *testing.T) {
	rng := newFixtureRNG(0xca5e)
	c := func(n int, alphabet string) string { return rng.chars(n, alphabet) }
	kv := func(key, value string) string { return key + ` = "` + value + `"` }
	upper36 := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	positives := []struct {
		name string
		body string
	}{
		{"grafana_cloud_api_token", kv("grafana", "GLC_"+c(40, alphaB64))},
		{"grafana_service_account_token", kv("grafana", "GLSA_"+c(32, alphaAlnum)+"_"+c(8, alphaHex))},
		{"npm_access_token", kv("npm", "NPM_"+c(36, alphaLower36))},
		{"slack_app_token", kv("slack", "XAPP-1-"+c(12, upper36)+"-123456789-"+c(16, alphaLower36))},
		{"slack_config_access_token", kv("slack", "XOXE.XOXB-1-"+c(164, upper36))},
		{"slack_config_access_token", kv("slack", "XOXE.XOXP-1-"+c(164, upper36))},
		{"slack_config_access_token", kv("slack", "xoxe.xoxp-1-"+c(164, upper36))},
	}
	for _, tc := range positives {
		if got := Scan([]byte(tc.body)); len(got) != 1 || got[0] != tc.name {
			t.Errorf("%s case-variant positive: Scan = %v, want exactly [%s]", tc.name, got, tc.name)
		}
	}

	negatives := map[string]string{
		"mixed-case glc_":        kv("grafana", "gLc_"+c(40, alphaB64)),
		"mixed-case glsa_":       kv("grafana", "Glsa_"+c(32, alphaAlnum)+"_"+c(8, alphaHex)),
		"mixed-case npm_":        kv("npm", "nPm_"+c(36, alphaLower36)),
		"mixed-case xapp-":       kv("slack", "xApp-1-"+c(12, upper36)+"-123456789-"+c(16, alphaLower36)),
		"uppercase eyJrIjoi":     kv("grafana", "EYJRIJOI"+c(80, alphaAlnum)+"="),
		"wildcard dot":           kv("slack", "xoxeXxoxb-1-"+c(164, upper36)),
		"jwt header not eyJ":     kv("token", "eyK"+c(20, alphaAlnum)+".eyJ"+c(20, alphaWord+"/")+"."+c(12, alphaWord+"/")),
		"jwt header too short J": kv("token", "eyJ"+c(5, alphaAlnum)+".eyJ"+c(20, alphaWord+"/")+"."+c(12, alphaWord+"/")),
	}
	for name, body := range negatives {
		if got := Scan([]byte(body)); len(got) != 0 {
			t.Errorf("%s: Scan = %v, want no hits", name, got)
		}
	}
}

// TestAwsAccessKeyIDEntropyFilter: aws_access_key_id carries the gitleaks
// 3.0 entropy threshold for its shape, so a regex-shaped but repetitive
// AKIA/ASIA window inside base64 attachment content (line-wrapped, so the
// \b anchors of the regex do see boundaries) is discarded, while a
// high-entropy window in the same surroundings is still claimed.
func TestAwsAccessKeyIDEntropyFilter(t *testing.T) {
	rng := newFixtureRNG(0xba64)
	blob := base64.StdEncoding.EncodeToString([]byte(rng.chars(256, alphaAlnum)))
	// Random base64 attachment content alone is clean.
	if got := Scan([]byte(blob)); len(got) != 0 {
		t.Fatalf("random base64 blob: Scan = %v, want no hits", got)
	}
	// Repetitive (low-entropy) AKIA-shaped window on its own wrapped line.
	low := blob[:40] + "\nAKIA" + strings.Repeat("AB", 8) + "\n" + blob[40:]
	if got := Scan([]byte(low)); len(got) != 0 {
		t.Errorf("low-entropy AKIA window in base64 content: Scan = %v, want no hits", got)
	}
	// High-entropy window in the same surroundings still reports.
	high := blob[:40] + "\nAKIA" + rng.chars(16, alphaUpper32) + "\n" + blob[40:]
	if got := Scan([]byte(high)); len(got) != 1 || got[0] != "aws_access_key_id" {
		t.Errorf("high-entropy AKIA window in base64 content: Scan = %v, want [aws_access_key_id]", got)
	}
}
