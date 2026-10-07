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

// The decided answers this file enforces. Every response the Handler writes carries the three
// headers below; an /api/ response is never cached; no response carries a CORS allow header,
// because the screen is served from this same origin and a cross-origin caller is refused
// instead.
//
// The one exception is every response net/http writes by itself before it calls the Handler,
// whatever the reason it refuses the request: for example a missing, repeated or malformed
// Host, an Expect other than 100-continue, an unsupported protocol version or transfer
// encoding, an oversized header, or a request line it cannot parse. net/http offers no hook
// to add a header to those, and this package deliberately adds no HTTP parser, no connection
// wrapper that rewrites response bytes and no second listener to reach them. The boundary is
// acceptable because a browser cannot make such a request (Host and Expect are forbidden
// header names in fetch), the body is only net/http's fixed status text, no path, static file
// or token is touched, and the connection is closed. internal/gui/listener_test.go pins it.
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
	// codeBadRequest answers a write whose body could not be read at all.
	codeBadRequest = "bad_request"
	codeInternal   = "internal"
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

// setSecurityHeaders sets the headers every response the Handler writes carries, before the
// status is written. A /api/ response is no-store; a static response keeps its own cache
// policy. A response net/http writes before it calls the Handler never reaches this function;
// the file-head comment names that exception and why it is accepted.
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

// absoluteForm reports whether the request line carried an absolute URI. net/http promotes that
// URI's authority to r.Host and drops the Host header, so an absolute-form request could name an
// allowed authority while its Host header names another host: the check would pass on a value
// the client did not send as a Host. This server takes origin-form only, so the form itself is
// refused before any host comparison.
func absoluteForm(r *http.Request) bool {
	return r.URL.Host != "" || strings.HasPrefix(strings.ToLower(r.RequestURI), "http://") || strings.HasPrefix(strings.ToLower(r.RequestURI), "https://")
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

// headerLines returns every line the named header field carries, and whether the field is
// present at all. http.Header is a map keyed by the canonical spelling, and the token header
// is sent as X-CRW-Token but stored as X-Crw-Token, so the lookup goes through Values, which
// canonicalises its argument; a raw map index would silently miss it. A field present with an
// empty value is present with one empty line, which the value checks refuse; a field with two
// lines is refused before any value is compared. This reads a header field, not r.Host:
// net/http removes Host from r.Header once it promotes it, so the Host check reads r.Host.
func headerLines(h http.Header, name string) (lines []string, present bool) {
	lines = h.Values(name)
	return lines, lines != nil
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
		writeError(w, r, http.StatusBadRequest, codeBadRequest)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}

// guard is the one security boundary: it sets the headers every response it writes carries,
// refuses a traversal path, checks the Host on every request, and checks the write rules
// (content type, token and, when present, Origin) for every method other than GET and HEAD. It
// runs before any route or static handler, so a later issue's handler cannot be reached without
// passing it. A response net/http writes before it calls this function is the one exception to
// the header contract; the file-head comment states why it is accepted.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, r)
		if absoluteForm(r) {
			writeError(w, r, http.StatusForbidden, codeForbidden)
			return
		}
		if traversalSegment(r) {
			writeError(w, r, http.StatusBadRequest, codeBadPath)
			return
		}
		if !hostAllowed(r.Host, s.port) {
			writeError(w, r, http.StatusForbidden, codeForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Each write header is counted by line as well as checked by value: a field with
			// two lines is refused even when every line is individually valid, because the
			// decided answer treats a repeated field as a malformed write rather than a
			// value to reconcile. A missing Content-Type or token fails the length check,
			// as its value check did before. An empty Origin is present with one empty line,
			// which originAllowed refuses; an absent Origin leaves the token to decide.
			contentTypes, _ := headerLines(r.Header, "Content-Type")
			if len(contentTypes) != 1 || !contentTypeIsJSON(contentTypes[0]) {
				writeError(w, r, http.StatusForbidden, codeForbidden)
				return
			}
			tokens, _ := headerLines(r.Header, tokenHeader)
			if len(tokens) != 1 || !tokenMatches(tokens[0], s.token) {
				writeError(w, r, http.StatusForbidden, codeForbidden)
				return
			}
			origins, originPresent := headerLines(r.Header, "Origin")
			if originPresent && (len(origins) != 1 || !originAllowed(origins[0], s.port)) {
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
