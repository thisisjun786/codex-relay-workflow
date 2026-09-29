package reading

import "unicode/utf8"

// FSDecode is os.fsdecode of a POSIX path as the Go string of Python's code points: UTF-8, with
// each byte that is not part of a valid sequence the lone surrogate U+DC00+byte
// (surrogateescape), held as WTF-8 - the form Decode gives a JSON "\udcXX" escape and the record
// encoder writes back as that escape. A path that is valid UTF-8 comes back unchanged.
func FSDecode(path string) string {
	if utf8.ValidString(path) {
		return path
	}
	out := make([]byte, 0, len(path)+len(path)/2)
	for i := 0; i < len(path); {
		r, size := utf8.DecodeRuneInString(path[i:])
		if r == utf8.RuneError && size == 1 {
			escaped := 0xdc00 + rune(path[i])
			out = append(out, 0xe0|byte(escaped>>12), 0x80|byte(escaped>>6)&0x3f, 0x80|byte(escaped)&0x3f)
		} else {
			out = append(out, path[i:i+size]...)
		}
		i += size
	}
	return string(out)
}

// FSEncode is os.fsencode of such a string: each WTF-8 surrogate U+DC80..U+DCFF back to the byte
// it escapes. ok is false for any other lone surrogate, which names no file (Python raises
// UnicodeEncodeError).
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
