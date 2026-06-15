package native

import (
	"archive/tar"
	"io/fs"
	"testing"
)

func TestTarHeaderZeroesOwnerAndTime(t *testing.T) {
	h := tarHeader("a.txt", tar.TypeReg, 0o644, 3, "")
	if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
		t.Errorf("owner leak: uid=%d gid=%d uname=%q gname=%q", h.Uid, h.Gid, h.Uname, h.Gname)
	}
	if !h.ModTime.IsZero() || !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() {
		t.Errorf("time leak: mod=%v acc=%v chg=%v", h.ModTime, h.AccessTime, h.ChangeTime)
	}
}

func TestTarHeaderPreservesExecMasksSpecial(t *testing.T) {
	if got := tarHeader("x", tar.TypeReg, 0o755, 0, "").Mode; got != 0o755 {
		t.Errorf("exec mode = %o, want 0755", got)
	}
	mode := fs.FileMode(0o755) | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	if got := tarHeader("x", tar.TypeReg, mode, 0, "").Mode; got != 0o755 {
		t.Errorf("special-bit mode = %o, want 0755 (special bits masked)", got)
	}
}
