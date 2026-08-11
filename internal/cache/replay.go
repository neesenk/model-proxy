package cache

import (
	"errors"
	"net/http"
)

// Replay writes the cached status, all header values, and body. It flushes each
// chunk so cached SSE retains streaming behavior.
func Replay(writer http.ResponseWriter, entry *Entry) error {
	if entry == nil {
		return nil
	}
	for name, values := range entry.header {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(entry.status)
	flusher, _ := writer.(http.Flusher)
	for offset := 0; offset < len(entry.body); {
		end := offset + 32*1024
		if end > len(entry.body) {
			end = len(entry.body)
		}
		written, err := writer.Write(entry.body[offset:end])
		offset += written
		if flusher != nil && written > 0 {
			flusher.Flush()
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return errors.New("cache replay made no write progress")
		}
	}
	return nil
}
