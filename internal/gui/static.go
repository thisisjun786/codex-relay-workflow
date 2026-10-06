package gui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// assetFS is the screen tree this binary carries. The embed is the only source of a static
// file: the server never builds an operating-system path, so a request cannot name a file
// outside this tree. `all:` keeps files whose names begin with a dot or an underscore, which
// a later issue's built assets may use.
//
//go:embed all:assets
var assetFS embed.FS

// indexFile is the SPA fallback: every path that is not a file in the tree serves it, which
// is how the client-side router handles its own routes.
const indexFile = "assets/index.html"

// contentTypes is the extension-to-type table for the embedded tree, kept in the shape the
// CXC server used so the built assets of a later issue slot in without a change here.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".webp":  "image/webp",
	".woff2": "font/woff2",
}

// serveStatic answers a request from the embedded tree, or with the index fallback. The path
// has already passed the guard's traversal check; this handler still joins only against the
// embedded tree, so it holds no operating-system path of its own.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = indexFile
	} else {
		name = path.Join("assets", name)
	}
	data, err := fs.ReadFile(s.assets, name)
	if err != nil {
		data, err = fs.ReadFile(s.assets, indexFile)
		if err != nil {
			writeError(w, r, http.StatusNotFound, codeNotFound)
			return
		}
		name = indexFile
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// contentTypeFor names the type of an embedded file by its extension, defaulting to a byte
// stream, as the CXC server did.
func contentTypeFor(name string) string {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		if value, ok := contentTypes[strings.ToLower(name[dot:])]; ok {
			return value
		}
	}
	return "application/octet-stream"
}
