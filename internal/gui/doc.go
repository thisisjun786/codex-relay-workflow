// Package gui is the `crw gui` local dashboard server: a loopback-only net/http server
// that serves the screen assets embedded in this binary and a JSON API.
//
// The package holds one security boundary every request passes before any handler runs
// (guard.go), a route registry a later issue extends with one file and an init()
// (registry.go), and the embedded static tree (static.go). It reads nothing from the
// operating-system filesystem at run time and writes no file of its own.
package gui
