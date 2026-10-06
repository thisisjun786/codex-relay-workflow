package gui

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The decided answers this file enforces. Every response carries the three headers below;
// an /api/ response is never cached; no response carries a CORS allow header, because the
// screen is served from this same origin and a cross-origin caller is refused instead.
const (
	cspHeader     = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'"
	referrerValue = "no-referrer"
	// maxBodyBytes is the 1 MiB write-body limit of the decided answer (CXC's own constant
	// was 1,000,000; this issue fixes 1 MiB exactly, so 1048576).
	maxBodyBytes = 1 << 20
	// tokenHeader is the header a write presents its per-run token in.
	tokenHeader = "X-CRW-Token"
	// apiPrefix marks the JSON API; only these responses are no-store.
	apiPrefix = "/api/"
)

// The short English error codes of the decided answer. A response body never carries an
// environment variable, a secret or the token: a rejection names only its class.
const (
	codeForbidden = "forbidden"
	codeBadPath   = "bad_path"
	codeTooLarge  = "too_large"
	codeNotFound  = "not_found"
	codeInternal  = "internal"
)

// jsonError is the whole error body shape: {"error":"<code>"} and a newline.
type jsonError struct {
	Error string `json:"error"`
}

// writeJSON answers one response with the security headers, the JSON content type and the
// encoded body. It is the only place a response is written, so the headers cannot be missed.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// writeError answers one short JSON error.
func writeError(w http.ResponseWriter, r *http.Request, status int, code string) {
	writeJSON(w, r, status, jsonError{Error: code})
}

// setSecurityHeaders sets the headers every response carries, before the status is written.
// A /api/ response is no-store; a static response keeps its own cache policy.
func setSecurityHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", cspHeader)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", referrerValue)
	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		w.Header().Set("Cache-Control", "no-store")
	}
}

// traversalSegment reports whether a request path holds a segment with two consecutive dots.
// It inspects the parsed path and every segment of the escaped path unescaped, so a segment
// spelled "%2e%2e" is refused as well as a literal "..". It never truncates at a "?" or "#":
// an encoded delimiter is part of the path (the parser only splits on a real one), so cutting
// there would hide the dots that follow it. The check runs for every request, not only a static
// one, so an API route is not reachable through a traversal path either.
func traversalSegment(r *http.Request) bool {
	segments := strings.Split(r.URL.Path, "/")
	for _, escaped := range strings.Split(r.URL.EscapedPath(), "/") {
		if decoded, err := url.PathUnescape(escaped); err == nil {
			segments = append(segments, decoded)
		} else {
			segments = append(segments, escaped)
		}
	}
	for _, segment := range segments {
		if strings.Contains(segment, "..") {
			return true
		}
	}
	return false
}

// allowedHosts is the two spellings of this loopback server, in the order the decided answer
// names them.
func allowedHosts(port int) [2]string {
	return [2]string{"127.0.0.1:" + strconv.Itoa(port), "localhost:" + strconv.Itoa(port)}
}

// hostAllowed reports whether a Host header names this server: only the loopback address or
// the localhost name, at this exact port, case-insensitively. A missing Host, another name or
// another port is refused (CXC passed a missing Host; the decided answer refuses it).
func hostAllowed(host string, port int) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host != strings.ToLower(allowedHosts(port)[0]) && host != strings.ToLower(allowedHosts(port)[1]) {
		return false
	}
	return true
}

// originAllowed reports whether a present Origin header names this server.
func originAllowed(origin string, port int) bool {
	origin = strings.ToLower(strings.TrimSpace(origin))
	for _, host := range allowedHosts(port) {
		if origin == "http://"+strings.ToLower(host) {
			return true
		}
	}
	return false
}

// contentTypeIsJSON reports whether a Content-Type is application/json, parameters allowed.
func contentTypeIsJSON(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// tokenMatches compares the presented token with the run token in constant time.
func tokenMatches(presented, token string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// readBody reads a write body up to the 1 MiB limit and leaves the bytes on the request for
// the handler. A declared Content-Length over the limit is refused at once (CXC's own
// pre-check, local-http.ts:44-49); the read itself is bounded, so a chunked body that lies
// about its length is refused too. The guard calls it before dispatch, which is what makes
// the 413 deterministic: a route that reads nothing still gets it, and a later issue's route
// cannot forget the check. It reports whether the request may continue; a refusal has already
// been written.
func readBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength > maxBodyBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, codeTooLarge)
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		// The target is request-local: a package-level one is written by every concurrent
		// request that reads past the limit, which is a data race.
		var limitErr *http.MaxBytesError
		if errors.As(err, &limitErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, codeTooLarge)
			return false
		}
		writeError(w, r, http.StatusBadRequest, codeInternal)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}

// guard is the one security boundary: it sets the headers every response carries, refuses a
// traversal path, checks the Host on every request, and checks the write rules (content type,
// token and, when present, Origin) for every method other than GET and HEAD. It runs before
// any route or static handler, so a later issue's handler cannot be reached without passing it.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, r)
		if traversalSegment(r) {
			writeError(w, r, http.StatusBadRequest, codeBadPath)
			return
		}
		if !hostAllowed(r.Host, s.port) {
			writeError(w, r, http.StatusForbidden, codeForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !contentTypeIsJSON(r.Header.Get("Content-Type")) {
				writeError(w, r, http.StatusForbidden, codeForbidden)
				return
			}
			if !tokenMatches(r.Header.Get(tokenHeader), s.token) {
				writeError(w, r, http.StatusForbidden, codeForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, s.port) {
				writeError(w, r, http.StatusForbidden, codeForbidden)
				return
			}
			if !readBody(w, r) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackPort reads the port a listener bound, which is the port the guard compares against.
func loopbackPort(addr net.Addr) (int, error) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return 0, errors.New("gui: the listener is not a TCP listener")
	}
	return tcp.Port, nil
}
