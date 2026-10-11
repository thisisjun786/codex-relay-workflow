// Package jsnode holds the text and number helpers that reproduce what Node.js does for the CXC port: Buffer.toString("utf8"),
// JSON.stringify, String.prototype.toLowerCase and Number.prototype.toString. It depends on nothing of this repository but
// internal/pyjson (the lone-surrogate spellings), so relay, harness, role, recall, contracttest and the pabcd packages can all import it.
// It has no package-level state and no cgo.
package jsnode
