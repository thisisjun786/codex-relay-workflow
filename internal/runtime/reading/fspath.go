package reading

import "unicode/utf8"

// FSEncode is os.fsencode of a string store.FSDecode makes (os.fsdecode of a POSIX path, each
// byte that is not UTF-8 the lone surrogate U+DC00+byte, held as WTF-8): each WTF-8 surrogate
// U+DC80..U+DCFF back to the byte it escapes. ok is false for any other lone surrogate, which
// names no file (Python raises UnicodeEncodeError).
func FSEncode(value string) (path string, ok bool) {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == 0xed && i+2 < len(value) && value[i+1] >= 0xa0 && value[i+1] <= 0xbf && value[i+2] >= 0x80 && value[i+2] <= 0xbf {
			r := rune(c&0x0f)<<12 | rune(value[i+1]&0x3f)<<6 | rune(value[i+2]&0x3f)
			if r < 0xdc80 || r > 0xdcff {
				return "", false
			}
			out = append(out, byte(r-0xdc00))
			i += 2
			continue
		}
		out = append(out, c)
	}
	return string(out), true
}

// WTF8 is whether s is UTF-8 in which a surrogate may stand (as FSDecode spells a byte that is
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
