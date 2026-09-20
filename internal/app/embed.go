package app

import (
	"embed"
	"io/fs"
)

// The frontend build output is embedded in the binary. The directory is
// gitignored and filled by `make embed` from web/dist — a plain go build
// needs that step first.
//
//go:embed all:web/dist
var webDist embed.FS

// FrontendFS exposes the embedded build. It returns an empty filesystem
// when no assets are bundled (tests).
func FrontendFS() fs.FS {
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil
	}
	return sub
}
