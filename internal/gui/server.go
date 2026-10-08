package gui

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/buildinfo"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// defaultShutdownTimeout is how long a cancelled run waits for in-flight requests before it
// ends. The decided answer says to wait briefly; this is the bound, and it is at least as long as a
// write's own post-publication phase (policystore.WriteSettleBound). A write that has already
// replaced the policy file must finish registering it: ending the process before that leaves the
// file and the wiring record naming different digests, and no bridge starts under those bytes.
var defaultShutdownTimeout = policystore.WriteSettleBound() + 5*time.Second

// Options is what a Server is built from. Every field has a default the zero value gets, so
// a caller supplies only what it must: Run supplies the port it bound, a test supplies the
// port, token and routes it wants to pin.
type Options struct {
	// Port is the port the server bound. The guard compares Host and Origin against it.
	Port int
	// Token is the per-run write token. Empty means a fresh one is generated.
	Token string
	// Version is what /api/health reports. Empty means the build version.
	Version string
	// Routes is the route table. Nil means the package-wide registry.
	Routes []Route
	// Assets is the static tree. Nil means the embedded tree.
	Assets fs.FS
	// ShutdownTimeout bounds the graceful shutdown. Zero means the default.
	ShutdownTimeout time.Duration
}

// Server is the loopback dashboard server. It owns the token, the bound port and the route
// table; ServeHTTP runs the guard before any handler.
type Server struct {
	port     int
	token    string
	env      *Env
	routes   map[string]Handler
	assets   fs.FS
	shutdown time.Duration
}

// New builds a Server. A duplicate method and path in the table is an error, which is how the
// decided answer makes a duplicate registration fail server start.
func New(opts Options) (*Server, error) {
	routes := opts.Routes
	if routes == nil {
		routes = Routes()
	}
	if duplicate := duplicateRoutes(routes); duplicate != "" {
		return nil, fmt.Errorf("gui: the route %s is already registered", duplicate)
	}
	token := opts.Token
	if token == "" {
		generated, err := newToken()
		if err != nil {
			return nil, err
		}
		token = generated
	}
	version := opts.Version
	if version == "" {
		version = buildinfo.Version
	}
	assets := opts.Assets
	if assets == nil {
		assets = assetFS
	}
	shutdown := opts.ShutdownTimeout
	if shutdown <= 0 {
		shutdown = defaultShutdownTimeout
	}
	table := make(map[string]Handler, len(routes))
	for _, route := range sortRoutes(routes) {
		table[route.Method+" "+route.Path] = route.Handler
	}
	return &Server{
		port:     opts.Port,
		token:    token,
		env:      &Env{Version: version},
		routes:   table,
		assets:   assets,
		shutdown: shutdown,
	}, nil
}

// Token is the write token this server requires. Run prints it in its URL line; a test uses
// it to build a request the guard accepts.
func (s *Server) Token() string { return s.token }

// Port is the port this server bound.
func (s *Server) Port() int { return s.port }

// Handler is the whole server as an http.Handler: the guard, then dispatch, then the static
// tree. Nothing reaches a route or an asset without passing the guard.
func (s *Server) Handler() http.Handler { return s.guard(http.HandlerFunc(s.dispatch)) }

// ServeHTTP serves one request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.Handler().ServeHTTP(w, r) }

// dispatch runs the registered route for this method and path, or answers a JSON 404 for an
// unregistered /api/ path, or serves the static tree. It runs after the guard.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if handler, ok := s.routes[r.Method+" "+r.URL.Path]; ok {
		response, err := handler(s.env, r)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, codeInternal)
			return
		}
		status := response.Status
		if status == 0 {
			status = http.StatusOK
		}
		if response.Body == nil {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, r, status, response.Body)
		return
	}
	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		writeError(w, r, http.StatusNotFound, codeNotFound)
		return
	}
	s.serveStatic(w, r)
}

// newToken makes the per-run write token: 32 random bytes in base64url, so it is safe in a
// URL fragment and long enough that guessing it is not a threat.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("gui: the write token cannot be generated: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
