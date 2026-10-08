package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

// hasDuplicateTopLevelKey reports whether the top-level object contains the
// key more than once. encoding/json keeps the LAST duplicate when unmarshaling
// into a struct/map, while the streaming fast paths return the FIRST — on
// duplicate keys the two are allowed to disagree, so differential asserts
// skip those bodies instead of pinning an arbitrary winner.
func hasDuplicateTopLevelKey(body []byte, key string) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return false
	}
	seen := false
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return false
		}
		if ks, ok := k.(string); ok && ks == key {
			if seen {
				return true
			}
			seen = true
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return false
		}
	}
	return false
}

// FuzzExtractModelDifferential pins the hot path's correctness: ExtractModel
// reads three tokens via a streaming decoder (653× faster than full
// unmarshal on real bodies) and its result must agree with a full
// encoding/json unmarshal on every VALID body without duplicate "model"
// keys. Malformed bodies only need to not panic (routing fails later at
// conversion anyway), and duplicate-key bodies are skipped by contract.
func FuzzExtractModelDifferential(f *testing.F) {
	seeds := []string{
		`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"model":"kimi-k3"}`,
		`{"model":"x","stream":true}`,
		`{"model":""}`,
		`{"model":null}`,
		`{"model":5}`,
		`{"other":true}`,
		`{}`,
		`{"model":"a","model":"b"}`, // duplicate: differential skips, panic-only
		`{"model":"a","model":"b","messages":[]}`,
		`{"a":{"model":"nested"},"model":"top"}`,
		`not json at all`,
		`{"model":"unterminated`,
		``,
		`[]`,
		`{"model":"big","pad":"` + string(bytes.Repeat([]byte("x"), 4096)) + `"}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		fast := ExtractModel(body)
		var ref struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &ref); err != nil {
			return
		}
		if hasDuplicateTopLevelKey(body, "model") {
			return
		}
		if fast != ref.Model {
			t.Fatalf("ExtractModel(%q) = %q, full unmarshal = %q", body, fast, ref.Model)
		}
	})
}

// FuzzRequestWantsStreamDifferential pins the other sonic fast path:
// requestWantsStream accepts ONLY a literal boolean true (no coercion), same
// as the retired map-unmarshal semantics. Agreement with a full
// encoding/json unmarshal on valid, duplicate-free bodies.
func FuzzRequestWantsStreamDifferential(f *testing.F) {
	seeds := []string{
		`{"model":"m","stream":true}`,
		`{"model":"m","stream":false}`,
		`{"stream":true}`,
		`{"stream":"true"}`,
		`{"stream":1}`,
		`{"stream":null}`,
		`{"stream":{}}`,
		`{"stream":true,"stream":false}`, // duplicate: differential skips
		`{"model":"m"}`,
		`{"stream":[true]}`,
		`garbage`,
		`{"stream":true}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		fast := requestWantsStream(body)
		var ref map[string]any
		if err := json.Unmarshal(body, &ref); err != nil {
			return
		}
		if hasDuplicateTopLevelKey(body, "stream") {
			return
		}
		want, _ := ref["stream"].(bool)
		if fast != want {
			t.Fatalf("requestWantsStream(%q) = %v, full unmarshal stream==true = %v", body, fast, want)
		}
	})
}
