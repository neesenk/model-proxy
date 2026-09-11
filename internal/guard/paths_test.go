package guard

import (
	"strings"
	"testing"
)

// Path fixtures reference well-known locations only; no real home directories
// or credentials are touched.

func TestScanPathsBuiltinCategories(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	cases := []struct {
		category string
		body     string
	}{
		{"ssh", "please cat ~/.ssh/id_rsa and summarize"},
		{"ssh", "my id_ed25519 key is broken"},
		{"aws_creds", "check ~/.aws/credentials for me"},
		{"gnupg", "list ~/.gnupg private keys"},
		{"kube", "read ~/.kube/config"},
		{"docker", "show ~/.docker/config.json"},
		{"gcloud", "open ~/.config/gcloud/application_default_credentials.json"},
		{"dotenv", "load the .env file"},
	}
	for _, tc := range cases {
		got := s.ScanPaths([]byte(tc.body))
		if len(got) != 1 || got[0] != tc.category {
			t.Errorf("ScanPaths(%q) = %v, want [%s]", tc.body, got, tc.category)
		}
	}
}

// dotenv requires path boundaries on both sides of ".env".
func TestScanPathsDotenvBoundaries(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	positives := []string{
		".env",
		"load .env please",
		"cat ./.env",
		"cat ~/.env",
		`open ".env"`,
		`open '.env'`,
		"read .env.local too",
		"see .env\nnext line",
	}
	for _, body := range positives {
		if got := s.ScanPaths([]byte(body)); len(got) != 1 || got[0] != "dotenv" {
			t.Errorf("ScanPaths(%q) = %v, want [dotenv]", body, got)
		}
	}
	negatives := []string{
		"foo.env",      // name ending in .env, no boundary before
		"foo.env.bar",  // .env in the middle of a name
		"environment",  // no dot at all
		".environment", // boundary after fails
		"my-.env-file", // '-' is a path char: both boundaries fail
		"app_env",      // no dot
	}
	for _, body := range negatives {
		if got := s.ScanPaths([]byte(body)); len(got) != 0 {
			t.Errorf("ScanPaths(%q) = %v, want no hits", body, got)
		}
	}
}

func TestScanPathsCleanBody(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	body := `{"model":"m","messages":[{"role":"user","content":"explain ssh tunneling"}]}`
	if got := s.ScanPaths([]byte(body)); len(got) != 0 {
		t.Errorf("ScanPaths(clean) = %v, want no hits", got)
	}
}

func TestScanPathsExtraPaths(t *testing.T) {
	s := mustScanner(t, nil, nil, []string{"~/.company/secrets", "  ", "~/.company/secrets"})
	if got := s.ScanPaths([]byte("open ~/.company/secrets/prod.yaml")); len(got) != 1 || got[0] != "custom_path" {
		t.Errorf("ScanPaths(extra) = %v, want [custom_path]", got)
	}
	if got := s.ScanPaths([]byte("nothing here")); len(got) != 0 {
		t.Errorf("ScanPaths(no extra hit) = %v, want no hits", got)
	}
}

// Paths are a signal only: Redact never rewrites them.
func TestPathsAreNotRedacted(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	body := []byte("please read ~/.ssh/id_rsa and the .env file")
	out := s.Redact(body)
	if string(out) != string(body) {
		t.Errorf("Redact rewrote a paths-only body: %q", out)
	}
}

// Multiple categories in one body report in table order, deduplicated.
func TestScanPathsOrderAndDedup(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	body := "~/.aws/credentials then ~/.ssh/id_rsa then ~/.ssh/config then .env"
	want := []string{"ssh", "aws_creds", "dotenv"}
	got := s.ScanPaths([]byte(body))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ScanPaths = %v, want %v", got, want)
	}
}

// id_rsa / id_ed25519 require dotenv-style path-char boundaries, so
// "did_rsakey" does not fire while "id_rsa.pub" still does.
func TestScanPathsSSHKeyFileBoundaries(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	positives := []string{
		"id_rsa",
		"read id_ed25519 please",
		"cat id_rsa.pub",
		`open "id_ed25519"`,
		"~/.ssh/id_rsa",
	}
	for _, body := range positives {
		got := s.ScanPaths([]byte(body))
		if len(got) != 1 || got[0] != "ssh" {
			t.Errorf("ScanPaths(%q) = %v, want [ssh]", body, got)
		}
	}
	negatives := []string{
		"did_rsakey is just an identifier",
		"my_id_rsa",
		"id_ed25519_backup",
		"xid_rsax",
	}
	for _, body := range negatives {
		if got := s.ScanPaths([]byte(body)); len(got) != 0 {
			t.Errorf("ScanPaths(%q) = %v, want no hits", body, got)
		}
	}
}

// The two ssh table entries (~/.ssh and the bounded key-file names) report
// the category once even when both fire.
func TestScanPathsSSHCategoryDedup(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	if got := s.ScanPaths([]byte("cat ~/.ssh/id_rsa")); len(got) != 1 || got[0] != "ssh" {
		t.Errorf("ScanPaths = %v, want exactly [ssh]", got)
	}
}
