//go:build dev

package ci

import (
	"net/netip"
	"regexp"
	"strings"
)

var ipvFuture = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)

// pyURLSplit returns urllib.parse.urlsplit(target)'s scheme, netloc and path, with the
// ValueError texts urlsplit raises for a malformed bracketed host.
func pyURLSplit(target string) (scheme, netloc, path string, err error) {
	target = strings.TrimLeft(target, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	target = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(target)
	if i := strings.Index(target, ":"); i > 0 && isASCIILetter(target[0]) {
		candidate := target[:i]
		if strings.Trim(candidate, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-.") == "" {
			scheme, target = strings.ToLower(candidate), target[i+1:]
		}
	}
	if strings.HasPrefix(target, "//") {
		end := len(target)
		for _, c := range "/?#" {
			if i := strings.IndexRune(target[2:], c); i >= 0 && i+2 < end {
				end = i + 2
			}
		}
		netloc, target = target[2:end], target[end:]
		open, close := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
		if open != close {
			return "", "", "", valueError{"Invalid IPv6 URL"}
		}
		if open && close {
			if err := checkBracketedNetloc(netloc); err != nil {
				return "", "", "", err
			}
		}
	}
	if i := strings.Index(target, "#"); i >= 0 {
		target = target[:i]
	}
	if i := strings.Index(target, "?"); i >= 0 {
		target = target[:i]
	}
	return scheme, netloc, target, nil
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func rpartition(s, sep string) (string, string) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):]
	}
	return "", s
}

func checkBracketedNetloc(netloc string) error {
	_, hostPort := rpartition(netloc, "@")
	before, bracketed, hasOpen := strings.Cut(hostPort, "[")
	var hostname string
	if hasOpen {
		if before != "" {
			return valueError{"Invalid IPv6 URL"}
		}
		var port string
		hostname, port, _ = strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return valueError{"Invalid IPv6 URL"}
		}
	} else {
		hostname, _, _ = strings.Cut(hostPort, ":")
	}
	if strings.HasPrefix(hostname, "v") {
		if !ipvFuture.MatchString(hostname) || strings.Contains(hostname, "\n") {
			return valueError{"IPvFuture address is invalid"}
		}
		return nil
	}
	addr, err := netip.ParseAddr(hostname)
	if err != nil {
		return valueError{pyRepr(hostname) + " does not appear to be an IPv4 or IPv6 address"}
	}
	if addr.Is4() {
		return valueError{"An IPv4 address cannot be in brackets"}
	}
	return nil
}

// pyHostname is urlsplit(...).hostname is not None and nonempty.
func pyHasHostname(netloc string) bool {
	_, hostinfo := rpartition(netloc, "@")
	if _, bracketed, open := strings.Cut(hostinfo, "["); open {
		hostname, _, _ := strings.Cut(bracketed, "]")
		return hostname != ""
	}
	hostname, _, _ := strings.Cut(hostinfo, ":")
	return hostname != ""
}

// httpsURL is plugin.py's https_url: an https URL with a host.
func httpsURL(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	scheme, netloc, _, err := pyURLSplit(text)
	return err == nil && scheme == "https" && pyHasHostname(netloc)
}
