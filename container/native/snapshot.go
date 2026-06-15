package native

import (
	"archive/tar"
	"io/fs"
	"os"
)

// tarHeader builds a DETERMINISTIC tar header: zero mtime/atime/ctime and zero
// owner identity (uid/gid/uname/gname), so identical workspace content yields a
// byte-identical archive (the blob = content-hash invariant). Never use
// tar.FileInfoHeader — it leaks the runner's uid/gid/uname/gname + real mtime.
// For reg/dir the mode is masked to permission bits (exec preserved;
// setuid/setgid/sticky stripped). Format is left unset so archive/tar picks the
// minimal per-entry format (USTAR for short paths, PAX only when a path is long
// or a file is large) — deterministic given zeroed time/owner, and unlike a
// forced USTAR it does not fail on long paths or files > 8 GiB.
func tarHeader(name string, typeflag byte, mode fs.FileMode, size int64, linkname string) *tar.Header {
	h := &tar.Header{Name: name, Typeflag: typeflag, Linkname: linkname}
	switch typeflag {
	case tar.TypeReg:
		h.Mode = int64(mode & os.ModePerm)
		h.Size = size
	case tar.TypeDir:
		h.Mode = int64(mode & os.ModePerm)
	case tar.TypeSymlink:
		h.Mode = 0o777 // conventional; symlink perms are ignored by the kernel
	}
	return h
}
