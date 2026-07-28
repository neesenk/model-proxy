package protocol

import "io"

func readAll(r io.Reader) []byte {
	body, _ := io.ReadAll(r)
	return body
}
