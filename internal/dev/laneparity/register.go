//go:build dev

package laneparity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// Registered is one hook registration as a host reads it from a plugin root.
type Registered struct {
	File    string `json:"file"`
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Async   bool   `json:"async,omitempty"`
	Status  string `json:"statusMessage,omitempty"`
	// Leg and EventToken are what the command starts (Route); Leg is empty for a command that starts no leg.
	Leg        string `json:"leg,omitempty"`
	EventToken string `json:"eventToken,omitempty"`
}

// Manifest is the part of .codex-plugin/plugin.json the harness reads.
type Manifest struct {
	Name  string
	Hooks []string
}

// ReadRegistered reads the hook registrations of a plugin root the way a host does: the manifest's
// hooks list, each file in it, each command entry. A hook file outside the root is refused.
func ReadRegistered(root string) (Manifest, []Registered, error) {
	var raw struct {
		Name  string          `json:"name"`
		Hooks json.RawMessage `json:"hooks"`
	}
	m, err := os.ReadFile(filepath.Join(root, ".codex-plugin", "plugin.json"))
	if err != nil {
		return Manifest{}, nil, err
	}
	if err := json.Unmarshal(m, &raw); err != nil {
		return Manifest{}, nil, fmt.Errorf("plugin.json: %w", err)
	}
	manifest := Manifest{Name: raw.Name}
	if len(raw.Hooks) > 0 {
		var one string
		if json.Unmarshal(raw.Hooks, &manifest.Hooks) != nil {
			if err := json.Unmarshal(raw.Hooks, &one); err != nil {
				return manifest, nil, fmt.Errorf("plugin.json hooks: neither a list of paths nor a path")
			}
			manifest.Hooks = []string{one}
		}
	}
	var out []Registered
	for _, rel := range manifest.Hooks {
		clean := filepath.Clean(strings.TrimPrefix(rel, "./"))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return manifest, nil, fmt.Errorf("plugin.json hooks: %q leaves the plugin root", rel)
		}
		data, err := os.ReadFile(filepath.Join(root, clean))
		if err != nil {
			return manifest, nil, fmt.Errorf("plugin.json hooks: %w", err)
		}
		var file hookFile
		if err := json.Unmarshal(data, &file); err != nil {
			return manifest, nil, fmt.Errorf("%s: %w", clean, err)
		}
		events := make([]string, 0, len(file.Hooks))
		for event := range file.Hooks {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			for _, group := range file.Hooks[event] {
				for _, h := range group.Hooks {
					if h.Type != "command" {
						continue
					}
					r := Registered{File: clean, Event: event, Matcher: group.Matcher, Command: h.Command, Timeout: h.Timeout, Async: h.Async, Status: h.StatusMessage}
					r.EventToken, r.Leg, _ = Route(h.Command)
					out = append(out, r)
				}
			}
		}
	}
	return manifest, out, nil
}

// LegCheck is the registration cell of one leg.
type LegCheck struct {
	Leg      string   `json:"leg"`
	OK       bool     `json:"ok"`
	Problems []string `json:"problems,omitempty"`
	// Command is the command line the root declares for the leg (empty when it declares none).
	Command string `json:"command,omitempty"`
}

// RegistrationReport is the registration cell of a plugin root.
type RegistrationReport struct {
	Plugin   string     `json:"plugin"`
	Legs     []LegCheck `json:"legs"`
	Extra    []string   `json:"extra,omitempty"`
	Problems []string   `json:"problems,omitempty"`
	OK       bool       `json:"ok"`
}

// CheckRegistration compares what a root declares with what it must declare. A leg is found by what
// its command starts, not by the file it sits in; a leg declared under another event, with another
// matcher, timeout or status message, declared twice, or not at all is a problem, and so is a
// registration that starts no leg of the table.
func CheckRegistration(manifest Manifest, got []Registered, want []Leg) RegistrationReport {
	rep := RegistrationReport{Plugin: manifest.Name, OK: true}
	if manifest.Name != PluginName {
		rep.Problems = append(rep.Problems, fmt.Sprintf("plugin.json name is %q, expected %q", manifest.Name, PluginName))
	}
	known := map[string]bool{}
	for _, l := range want {
		known[l.Leg] = true
		check := LegCheck{Leg: l.Leg}
		var hits []Registered
		for _, r := range got {
			if r.Leg == l.Leg {
				hits = append(hits, r)
			}
		}
		switch len(hits) {
		case 0:
			check.Problems = append(check.Problems, "no registration starts this leg")
		case 1:
			r := hits[0]
			check.Command = r.Command
			add := func(format string, args ...any) {
				check.Problems = append(check.Problems, fmt.Sprintf(format, args...))
			}
			if r.Event != l.Event {
				add("registered under event %s, expected %s", r.Event, l.Event)
			}
			if r.EventToken != cxccorpus.Kebab(l.Event) && l.Leg != CompletionLeg {
				add("command passes event %q, expected %q", r.EventToken, cxccorpus.Kebab(l.Event))
			}
			if r.Async {
				add("registered async: the host does not wait for its answer")
			}
			if l.Own {
				if r.Timeout <= 0 {
					add("no timeout")
				}
				if l.Matcher != "" && r.Matcher != l.Matcher {
					add("matcher %q, expected %q", r.Matcher, l.Matcher)
				}
			} else {
				if r.Matcher != l.Matcher {
					add("matcher %q, expected %q", r.Matcher, l.Matcher)
				}
				if r.Timeout != l.Timeout {
					add("timeout %d, expected %d", r.Timeout, l.Timeout)
				}
				if r.Status != l.Status {
					add("statusMessage %q, expected %q", r.Status, l.Status)
				}
			}
		default:
			check.Command = hits[0].Command
			check.Problems = append(check.Problems, fmt.Sprintf("registered %d times", len(hits)))
		}
		check.OK = len(check.Problems) == 0
		rep.OK = rep.OK && check.OK
		rep.Legs = append(rep.Legs, check)
	}
	for _, r := range got {
		if !known[r.Leg] {
			desc := fmt.Sprintf("%s %s: %s", r.File, r.Event, r.Command)
			if r.Leg == "" {
				desc += " (starts no leg of the table)"
			} else {
				desc += fmt.Sprintf(" (leg %q is not in the table)", r.Leg)
			}
			rep.Extra = append(rep.Extra, desc)
		}
	}
	rep.OK = rep.OK && len(rep.Problems) == 0 && len(rep.Extra) == 0
	return rep
}

// Declared maps each leg a root declares exactly once, under the event the leg belongs to, to its
// command line: what the host would run when that event fires. A registration under another event
// is not run for this leg's payloads, and a leg declared twice is ambiguous; both are absent.
func Declared(got []Registered, want []Leg) map[string]string {
	out := map[string]string{}
	for leg, r := range DeclaredRegistrations(got, want) {
		out[leg] = r.Command
	}
	return out
}

// DeclaredRegistrations is Declared with the whole registration of each leg: the latency cell holds
// a leg to the timeout the root declares for it.
func DeclaredRegistrations(got []Registered, want []Leg) map[string]Registered {
	event := map[string]string{}
	for _, l := range want {
		event[l.Leg] = l.Event
	}
	count := map[string]int{}
	for _, r := range got {
		if r.Leg != "" {
			count[r.Leg]++
		}
	}
	out := map[string]Registered{}
	for _, r := range got {
		if r.Leg != "" && count[r.Leg] == 1 && r.Event == event[r.Leg] {
			out[r.Leg] = r
		}
	}
	return out
}
