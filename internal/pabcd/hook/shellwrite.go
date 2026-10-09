package hook

import (
	"unicode/utf16"
)

func shellString(s []uint16) string { return string(utf16.Decode(s)) }
