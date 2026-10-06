package gui

import (
	"net/http"
	"sort"
	"sync"
)

// Env is what a route handler runs with: the values the server knows that a handler needs.
// It carries no secret, so a handler cannot put one in a response by accident.
type Env struct {
	Version string
}

// Response is what a route handler returns: the status and a value that is encoded as JSON.
type Response struct {
	Status int
	Body   any
}

// Handler serves one registered route. It receives the request after the guard has run, so
// it may read the body; it returns a Response or an error, which the server answers as a
// 500 with a short error code.
type Handler func(env *Env, r *http.Request) (Response, error)

// Route is one registered method and path.
type Route struct {
	Method  string
	Path    string
	Handler Handler
}

// registry is every route a file registered from its init(). A later issue adds its own
// file with an init() that calls Register, so it never edits the command table or this file.
var (
	registryMu sync.Mutex
	registry   []Route
)

// Register adds a route to the package-wide table. It is called from init(), so it cannot
// return an error; a duplicate method and path is refused when the server is built
// (duplicateRoutes), which is the point the decided answer makes the failure land on.
func Register(method, path string, handler Handler) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = append(registry, Route{Method: method, Path: path, Handler: handler})
}

// Routes is the registered routes in registration order, as a copy: a caller cannot change
// the table the server reads.
func Routes() []Route {
	registryMu.Lock()
	defer registryMu.Unlock()
	return append([]Route(nil), registry...)
}

// duplicateRoutes names the first method and path that appears twice, or "" when every route
// is distinct. The comparison is exact: a same path under another method is not a duplicate.
func duplicateRoutes(routes []Route) string {
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if seen[key] {
			return key
		}
		seen[key] = true
	}
	return ""
}

// sortRoutes orders routes by path then method, so the dispatch table is deterministic
// whatever order the registering files loaded in.
func sortRoutes(routes []Route) []Route {
	out := append([]Route(nil), routes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}
