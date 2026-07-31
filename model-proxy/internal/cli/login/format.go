package login

import (
	"os"
)

// truncate caps a string at n bytes (provider display rules).

// HomeDir resolves the user home (credential files live under ~/.model-proxy).
func HomeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
