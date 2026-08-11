package app

import (
	"io"
	"net/http"
	"sync"

	"model-proxy/provider"
)

// fakeProviderImpl is the shared no-op provider implementation for app tests.
type fakeProviderImpl struct {
	rewritePath string
	mu          sync.Mutex
}

func (f *fakeProviderImpl) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer TEST")
	return nil
}
func (f *fakeProviderImpl) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	f.mu.Lock()
	f.rewritePath = path
	f.mu.Unlock()
	return targetURL, body
}
func (f *fakeProviderImpl) Refresh() error                 { return nil }
func (f *fakeProviderImpl) Logout() error                  { return nil }
func (f *fakeProviderImpl) Usage() error                   { return nil }
func (f *fakeProviderImpl) FetchModels() ([]string, error) { return nil, nil }
func (f *fakeProviderImpl) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingUnknown}, nil
}
func (f *fakeProviderImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{
		Method: http.MethodPost,
		Path:   "/chat/completions",
		Body:   []byte(`{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":false}`),
	}
}
func (f *fakeProviderImpl) ExtraHeaders(req *http.Request, path string)          {}
func (f *fakeProviderImpl) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

// readAll is a tiny test helper (io.ReadAll without the import noise at call sites).
func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}
