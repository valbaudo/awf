package live

import (
	"os"
	"path/filepath"
	"syscall"
)

func withSessionLock(root Root, adapterRef, sessionKey string, fn func() error) error {
	if err := ValidateSessionKey(sessionKey); err != nil {
		return err
	}
	path := lockPath(root, adapterRef, sessionKey)
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return wrapIO("open session lock", err)
	}
	defer func() {
		_ = file.Close()
	}()
	if err := file.Chmod(0o600); err != nil {
		return wrapIO("chmod session lock", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return wrapIO("lock session", err)
	}
	defer func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}()
	return fn()
}

func lockPath(root Root, adapterRef, sessionKey string) string {
	return filepath.Join(root.Path, "locks", escapePathComponent(adapterRef), escapePathComponent(sessionKey)+".lock")
}
