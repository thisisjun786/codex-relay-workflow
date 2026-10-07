// Package gui is the `crw gui` local dashboard server: a loopback-only net/http server
// that serves the screen assets embedded in this binary and a JSON API.
//
// The package holds one security boundary every request passes before any handler runs
// (guard.go), a route registry a later issue extends with one file and an init()
// (registry.go), and the embedded static tree (static.go). It reads nothing from the
// operating-system filesystem at run time and writes no file of its own.
//
// Every response the Handler writes carries `Content-Security-Policy: default-src 'self';
// frame-ancestors 'none'; base-uri 'none'`, `X-Content-Type-Options: nosniff` and
// `Referrer-Policy: no-referrer`, and an /api/ response also carries `Cache-Control:
// no-store`. The sole exception is every response net/http writes by itself before it calls
// the Handler, whatever the reason it refuses the request: for example a missing, repeated or
// malformed Host, an Expect other than 100-continue, an unsupported protocol version or
// transfer encoding, an oversized header, or a request line it cannot parse. net/http exposes
// no hook to add a header to those, and this package adds no HTTP parser, no connection
// wrapper that rewrites response bytes and no second listener to reach them. That boundary is
// acceptable because a browser cannot make such a request (Host and Expect are forbidden
// header names in fetch), the body is only net/http's fixed status text, no path, static file
// or token is touched, and the connection is closed. internal/gui/listener_test.go pins it.
package gui
