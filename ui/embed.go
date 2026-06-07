package ui

import (
	"embed"
	"io/fs"
)

// distFS holds the committed Vite build of the SPA (ui/dist). Rebuild with `make ui`
// after changing anything under ui/src. `all:` includes dotfiles/underscore files so
// the bundle is embedded verbatim.
//
//go:embed all:dist
var distFS embed.FS

// dist returns the embedded SPA rooted at dist/ (so "/" serves dist/index.html).
func dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Unreachable: dist is embedded at compile time. A panic here means the
		// build embedded a malformed tree.
		panic("ui: embedded dist subtree missing: " + err.Error())
	}
	return sub
}
