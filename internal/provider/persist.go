package provider

import (
	"os"
	"path/filepath"
)

// atomicWriteFile writes credential-bearing files via a unique temp file in
// the target directory followed by rename. OAuth/SSO stores are rewritten
// with ROTATED tokens mid-flight: a crash during a direct write leaves a
// truncated file where the old refresh token is already invalidated upstream
// and the new one was never persisted — the account is locked until a full
// re-login (docs/engineering/pitfalls.md #18).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
