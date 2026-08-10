package main

import (
	"log"

	"model-proxy/internal/observe/requestlog"
)

// initRequestLog adapts resolved configuration values into the process-owned
// logger. The goroutine itself remains owned by proxyLifecycle.
func (p *Proxy) initRequestLog(config RequestLogConfig) {
	if !config.Enabled {
		return
	}
	p.reqLog = requestlog.New(requestlog.Options{
		Directory:    config.ResolvedDir(),
		MaxFileSize:  config.MaxFileSizeBytes(),
		MaxBodyBytes: config.MaxBodyBytesValue(),
		Retention:    config.RetentionDuration(),
	})
	log.Printf(
		"[request_log] enabled -> %s (max_file_size %d bytes, max_body %d bytes, retention %s)",
		config.ResolvedDir(),
		config.MaxFileSizeBytes(),
		config.MaxBodyBytesValue(),
		config.RetentionDuration(),
	)
}
