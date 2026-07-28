package main

import responsecache "model-proxy/internal/cache"

// newResponseCache adapts resolved application configuration into the
// repository-leaf cache component.
func newResponseCache(config CacheConfig) *responsecache.Store {
	if !config.IsEnabled() {
		return nil
	}
	return responsecache.New(responsecache.Options{
		TTL:          config.TTLDuration(),
		MaxEntries:   config.MaxEntriesValue(),
		MaxBodyBytes: config.MaxBodyBytesValue(),
	})
}
