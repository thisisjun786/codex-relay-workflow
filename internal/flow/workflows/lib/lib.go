// Package lib embeds the helper library workflow scripts import as crw:lib/<name>.
package lib

import (
	"embed"
	"io/fs"
)

//go:embed *.js
var files embed.FS

// FS is the embedded library: crw:lib/<name> reads <name>.js from its root.
var FS fs.FS = files
