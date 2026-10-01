package reading

import "unicode/utf8"

// WTF8 is whether s is UTF-8 in which a surrogate may stand (as pyvalue.FSDecode spells a byte that is
// not UTF-8): every byte decodes, or is part of a three-byte surrogate sequence.
func WTF8(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if s[i] == 0xed && i+2 < len(s) && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2] >= 0x80 && s[i+2] <= 0xbf {
				i += 3
				continue
			}
			return false
		}
		i += size
	}
	return true
}
