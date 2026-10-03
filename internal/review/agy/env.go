package agy

import "strings"

// passEnv names the variables agy takes from the caller's environment by exact name; XDG_* and LC_* pass by prefix. Nothing else does.
var passEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LANGUAGE": true, "TZ": true, "TERM": true, "TMPDIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
}

// scrubEnv is environ cut down to the allowlist, followed by extra (later entries of the same name win in os/exec).
func scrubEnv(environ, extra []string) []string {
	out := make([]string, 0, len(environ)+len(extra))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if passEnv[name] || strings.HasPrefix(name, "XDG_") || strings.HasPrefix(name, "LC_") {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}
