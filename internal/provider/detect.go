// Package provider ports CXC v0.2.40 provider-bridge/src/detect.ts (18-101).
package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

type Status struct {
	Mode, OcxPath, Reason string
	Running               bool
	DefaultProvider       *string
	Port                  *float64
}

// Deps retains the oracle's optional resolver and status-reader seams.
type Deps struct {
	Which     func(string) string
	RunStatus func(string) (*int, string, error)
}

func Detect(d Deps) Status {
	path := ""
	if d.Which != nil {
		path = d.Which("ocx")
	}
	if path == "" {
		return Status{Mode: "native", Reason: "ocx not found on PATH; using native Codex catalog"}
	}
	fail := func(reason string) Status { return Status{Mode: "error", OcxPath: path, Reason: reason} }
	if d.RunStatus == nil {
		return fail("ocx detected but no status reader available")
	}
	code, out, err := d.RunStatus(path)
	if err != nil {
		return fail("ocx status invocation threw: " + err.Error())
	}
	if code == nil {
		return fail("ocx status exited null")
	}
	if *code != 0 {
		return fail(fmt.Sprintf("ocx status exited %d", *code))
	}
	s, ok := parseStatus(out)
	if !ok {
		return fail("ocx status produced no parseable payload")
	}
	s.Mode, s.OcxPath = "provider", path
	return s
}

func parseStatus(out string) (Status, bool) {
	d := json.NewDecoder(strings.NewReader(text.Trim(out)))
	d.UseNumber()
	var obj map[string]any
	if d.Decode(&obj) != nil {
		return Status{}, false
	}
	if _, err := d.Token(); err != io.EOF {
		return Status{}, false
	}
	proxy, _ := obj["proxy"].(map[string]any)
	running, ok := proxy["running"].(bool)
	if !ok {
		return Status{}, false
	}
	s := Status{Running: running}
	if provider, ok := obj["defaultProvider"].(string); ok {
		s.DefaultProvider = &provider
	}
	listen, _ := obj["listen"].(map[string]any)
	if n, ok := listen["port"].(json.Number); ok {
		f, _ := strconv.ParseFloat(string(n), 64)
		if !math.IsInf(f, 0) {
			s.Port = &f
		}
	}
	return s, true
}

// Line keeps JSON.stringify's insertion order and string/number spelling.
func Line(s Status) string {
	head := "{\"provider\":\"ocx\",\"mode\":" + quote(s.Mode)
	if s.Mode == "native" {
		return head + ",\"reason\":" + quote(s.Reason) + "}"
	}
	head += ",\"ocxPath\":" + quote(s.OcxPath)
	if s.Mode == "error" {
		return head + ",\"reason\":" + quote(s.Reason) + "}"
	}
	provider, port := "null", "null"
	if s.DefaultProvider != nil {
		provider = quote(*s.DefaultProvider)
	}
	if s.Port != nil {
		n := *s.Port + 0 // JSON.stringify writes negative zero as zero.
		if !math.IsInf(n, 0) && !math.IsNaN(n) {
			b, _ := json.Marshal(n)
			port = string(b)
		}
	}
	return head + ",\"running\":" + strconv.FormatBool(s.Running) + ",\"defaultProvider\":" + provider + ",\"port\":" + port + "}"
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
