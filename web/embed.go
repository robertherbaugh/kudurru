// Package web embeds the dashboard single-page application
// (docs/architecture.md §10, decision D69) and serves it.
//
// The bundle is built by Bun and Vite into web/dist (`make web-build`). A
// placeholder web/dist/index.html is committed so that `go build ./...`
// works on a checkout without Bun; release builds and CI build the real
// bundle first. web/dist/assets is ignored by git. The build stores
// compressible files as <name>.gz only (web/scripts/compress.ts), so the
// binary embeds the compressed bytes; Handler serves them.
//
// Mounting (cmd/kudurru/serve.go): register the API routes first, then
//
//	mux.Handle("/", web.Handler(web.Options{BasePath: prefix}))
//
// or, behind a path prefix,
//
//	mux.Handle(prefix+"/", http.StripPrefix(prefix, web.Handler(web.Options{BasePath: prefix})))
//
// so that unknown paths fall back to index.html (client-side routes) and
// everything under /api/v1 stays with the API.
package web

import (
	"bytes"
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS is the built dashboard rooted at the bundle directory: index.html at
// the top, hashed files under assets/.
var FS fs.FS = mustSub(dist, "dist")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic("web: " + err.Error())
	}
	return sub
}

// placeholderMarker is present only in the committed placeholder index.html.
const placeholderMarker = "<!-- kudurru:placeholder -->"

// Built reports whether FS holds a real bundle rather than the placeholder
// page committed for Bun-less builds.
func Built() bool {
	return isBuilt(FS)
}

func isBuilt(f fs.FS) bool {
	b, err := fs.ReadFile(f, "index.html")
	if err != nil {
		return false
	}
	return !bytes.Contains(b, []byte(placeholderMarker))
}
