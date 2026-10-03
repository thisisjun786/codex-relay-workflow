package recall

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parseRemoteURL is the bounded WHATWG subset consumed by repo-key.ts:44-46.
// It produces hostname and decoded pathname only. The explicitly declared UTS46
// platform differences live in known-defects; net/url has different identity rules.
func parseRemoteURL(raw string) (host, path string, ok bool) {
	raw = strings.TrimFunc(raw, func(r rune) bool { return r <= 0x20 })
	raw = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, raw)
	colon := strings.IndexByte(raw, ':')
	if colon < 1 || !asciiLetter(raw[0]) {
		return "", "", false
	}
	for i := 1; i < colon; i++ {
		if !asciiLetter(raw[i]) && (raw[i] < '0' || raw[i] > '9') && !strings.ContainsRune("+.-", rune(raw[i])) {
			return "", "", false
		}
	}
	scheme, rest := strings.ToLower(raw[:colon]), raw[colon+1:]
	special := scheme == "ftp" || scheme == "file" || scheme == "http" || scheme == "https" || scheme == "ws" || scheme == "wss"
	if scheme == "file" {
		rest = strings.ReplaceAll(rest, "\\", "/")
	}
	if special && scheme != "file" {
		rest = strings.TrimLeft(rest, "/\\")
	} else {
		if !strings.HasPrefix(rest, "//") {
			return "", "", false
		}
		rest = rest[2:]
	}
	stop := "/?#"
	if special {
		stop += "\\"
	}
	end := strings.IndexAny(rest, stop)
	if end < 0 {
		end = len(rest)
	}
	authority, path := rest[:end], rest[end:]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		if scheme == "file" {
			return "", "", false
		}
		authority = authority[at+1:]
	}
	host, ok = remoteAuthority(authority, special, scheme == "file")
	if !ok {
		return "", "", false
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = remotePath(path, special, scheme == "file")
	path, ok = strictPercentDecode(path)
	return host, path, ok
}

func asciiLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func remoteAuthority(s string, special, file bool) (string, bool) {
	if s == "" || file && driveLetter(s) {
		return "", false
	}
	host, port := s, ""
	if s[0] == '[' {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", false
		}
		tail := s[end+1:]
		if tail != "" {
			if file || tail[0] != ':' {
				return "", false
			}
			port = tail[1:]
		}
		if !validRemotePort(port) {
			return "", false
		}
		addr, err := netip.ParseAddr(s[1:end])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", false
		}
		return serializeIPv6(addr), true
	}
	if at := strings.IndexByte(s, ':'); at >= 0 {
		if file {
			return "", false
		}
		host, port = s[:at], s[at+1:]
	}
	if host == "" || !validRemotePort(port) {
		return "", false
	}
	if !special {
		return opaqueRemoteHost(host)
	}
	host, ok := strictPercentDecode(host)
	if !ok {
		return "", false
	}
	for _, r := range host {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune("%#/:<>?@[\\]^|", r) {
			return "", false
		}
	}
	labels := strings.Split(host, ".")
	for i, label := range labels {
		// Lower each rune separately: UTS46 never applies JS's contextual Final_Sigma.
		var b strings.Builder
		for _, r := range label {
			b.WriteString(Lower(string(r)))
		}
		label = b.String()
		if !isASCII(label) {
			var valid bool
			label, valid = punycodeLabel(label)
			if !valid {
				return "", false
			}
			label = "xn--" + label
		}
		labels[i] = label
	}
	host = strings.Join(labels, ".")
	if file && host == "localhost" {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	last := parts[len(parts)-1]
	_, number := ipv4Number(last)
	if number || last != "" && strings.Trim(last, "0123456789") == "" {
		return parseIPv4(parts)
	}
	return host, true
}

func validRemotePort(s string) bool {
	if s == "" {
		return true
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		n = n*10 + int(s[i]-'0')
		if n > 65535 {
			return false
		}
	}
	return true
}

func opaqueRemoteHost(s string) (string, bool) {
	var b strings.Builder
	const digits = "0123456789ABCDEF"
	for _, r := range s {
		if r == 0 || strings.ContainsRune("\t\n\r #/:<>?@[\\]^|", r) {
			return "", false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		} else {
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func ipv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	base := 10
	if len(s) >= 2 && s[0] == '0' {
		if s[1] == 'x' || s[1] == 'X' {
			base, s = 16, s[2:]
		} else {
			base, s = 8, s[1:]
		}
	}
	if s == "" {
		return 0, true
	}
	// ParseUint accepts a leading +; WHATWG's radix-number grammar does not.
	if strings.ContainsAny(s, "+-_") {
		return 0, false
	}
	// ParseUint can report overflow before examining a trailing invalid digit.
	// Validate the entire grammar first; such a label is a domain, not an IPv4 number.
	for i := 0; i < len(s); i++ {
		digit, valid := hexDigit(s[i])
		if !valid || int(digit) >= base {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, base, 64)
	// A syntactically valid oversized number still triggers IPv4 parsing, which
	// rejects it by the address range instead of accepting it as a domain label.
	return n, err == nil || errors.Is(err, strconv.ErrRange)
}

func parseIPv4(parts []string) (string, bool) {
	if len(parts) > 4 {
		return "", false
	}
	var value uint64
	for i, part := range parts {
		n, ok := ipv4Number(part)
		if !ok || i < len(parts)-1 && n > 255 {
			return "", false
		}
		if i == len(parts)-1 {
			if n >= uint64(1)<<uint(8*(5-len(parts))) {
				return "", false
			}
			value += n
		} else {
			value += n << uint(8*(3-i))
		}
	}
	return strconv.FormatUint(value>>24, 10) + "." + strconv.FormatUint(value>>16&255, 10) + "." + strconv.FormatUint(value>>8&255, 10) + "." + strconv.FormatUint(value&255, 10), true
}

func serializeIPv6(addr netip.Addr) string {
	b := addr.As16()
	var p [8]uint16
	for i := range p {
		p[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	start, length := -1, 1
	for i := 0; i < 8; {
		if p[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && p[j] == 0 {
			j++
		}
		if j-i > length {
			start, length = i, j-i
		}
		i = j
	}
	var out strings.Builder
	for i := 0; i < 8; i++ {
		if i == start {
			out.WriteString("::")
			i += length - 1
			continue
		}
		if i > 0 && i != start+length {
			out.WriteByte(':')
		}
		out.WriteString(strconv.FormatUint(uint64(p[i]), 16))
	}
	return "[" + out.String() + "]"
}

func driveLetter(s string) bool {
	return len(s) == 2 && asciiLetter(s[0]) && (s[1] == ':' || s[1] == '|')
}

func remotePath(s string, special, file bool) string {
	if special {
		s = strings.ReplaceAll(s, "\\", "/")
	}
	if s == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(s, "/"), "/")
	stack := []string{}
	for i, part := range parts {
		dot := strings.ToLower(part)
		dot = strings.ReplaceAll(dot, "%2e", ".")
		if dot == "." || dot == ".." {
			if dot == ".." && len(stack) > 0 && !(file && len(stack) == 1 && driveLetter(stack[0])) {
				stack = stack[:len(stack)-1]
			}
			if i == len(parts)-1 {
				stack = append(stack, "")
			}
		} else {
			if file && len(stack) == 0 && driveLetter(part) {
				part = part[:1] + ":"
			}
			stack = append(stack, part)
		}
	}
	return "/" + strings.Join(stack, "/")
}

func strictPercentDecode(s string) (string, bool) {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b = append(b, s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		hi, a := hexDigit(s[i+1])
		lo, c := hexDigit(s[i+2])
		if !a || !c {
			return "", false
		}
		b = append(b, hi<<4|lo)
		i += 2
	}
	return string(b), utf8.Valid(b)
}

func hexDigit(c byte) (byte, bool) {
	if c >= '0' && c <= '9' {
		return c - '0', true
	}
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 10, true
	}
	if c >= 'A' && c <= 'F' {
		return c - 'A' + 10, true
	}
	return 0, false
}

// RFC3492 section 6.3 encoder. Constants and bias adaptation are the Bootstring
// parameters of section 5; int64 keeps intermediate deltas bounded before use.
func punycodeLabel(s string) (string, bool) {
	rs := []rune(s)
	var b strings.Builder
	for _, r := range rs {
		if r < 128 {
			b.WriteByte(byte(r))
		}
	}
	basic := b.Len()
	handled := basic
	if basic > 0 {
		b.WriteByte('-')
	}
	n, delta, bias := int64(128), int64(0), int64(72)
	adapt := func(d, points int64, first bool) int64 {
		if first {
			d /= 700
		} else {
			d /= 2
		}
		d += d / points
		k := int64(0)
		for d > 455 {
			d /= 35
			k += 36
		}
		return k + 36*d/(d+38)
	}
	digit := func(d int64) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	for handled < len(rs) {
		m := int64(0x110000)
		for _, r := range rs {
			if int64(r) >= n && int64(r) < m {
				m = int64(r)
			}
		}
		delta += (m - n) * int64(handled+1)
		n = m
		if delta > 0x7fffffff {
			return "", false
		}
		for _, r := range rs {
			if int64(r) < n {
				delta++
				if delta > 0x7fffffff {
					return "", false
				}
			}
			if int64(r) != n {
				continue
			}
			q := delta
			for k := int64(36); ; k += 36 {
				threshold := max(int64(1), min(int64(26), k-bias))
				if q < threshold {
					break
				}
				b.WriteByte(digit(threshold + (q-threshold)%(36-threshold)))
				q = (q - threshold) / (36 - threshold)
			}
			b.WriteByte(digit(q))
			bias = adapt(delta, int64(handled+1), handled == basic)
			delta = 0
			handled++
		}
		delta++
		n++
	}
	return b.String(), true
}
