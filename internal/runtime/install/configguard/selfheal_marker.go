package configguard

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

const SelfHealMarkerName = "crw-self-heal.json"

// Pointers distinguish absent fields from false, zero and empty strings. A nil slice
// is absent; a nonnil empty slice is an explicitly recorded empty list.
type SelfHealMarker struct {
	OptedOut, AllEnabled   *bool
	OptedOutAt, CheckedAt  *string
	ConfigMtimeMs          *float64
	HealedKeys, CachedKeys []string
	// Probe is the verified execution evidence an explicit command recorded (CRW-1150).
	Probe *SelfHealProbeEvidence
	// probeSeen is true when the marker read carried a probeEvidence field, parsed or not: such a
	// marker is never judged by the legacy mtime cache (CRW-1150).
	probeSeen bool
	order     []string
}

func SelfHealMarkerPath(home string) string { return filepath.Join(home, SelfHealMarkerName) }

// ParseSelfHealMarker normalizes the oracle's accepted fields; malformed/non-object
// input is a cache miss. Strings retain lone UTF-16 surrogates as in JSON.parse.
func ParseSelfHealMarker(raw string) *SelfHealMarker {
	v, err := pyjson.Loads(raw, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil {
		return nil
	}
	o, ok := v.(pyjson.Object)
	if !ok {
		return nil
	}
	m := &SelfHealMarker{}
	if b, ok := o.Get("optedOut").(bool); ok {
		m.OptedOut = &b
	}
	m.OptedOutAt = manifestString(o.Get("optedOutAt"))
	if n, ok := manifestNumber(o.Get("configMtimeMs")); ok {
		m.ConfigMtimeMs = &n
	}
	m.CheckedAt = manifestString(o.Get("checkedAt"))
	if b, ok := o.Get("allEnabled").(bool); ok {
		m.AllEnabled = &b
	}
	stringsOnly := func(v any) []string {
		list, ok := v.([]any)
		if !ok {
			return nil
		}
		out := []string{}
		for _, v := range list {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	m.HealedKeys = stringsOnly(o.Get("healedKeys"))
	m.CachedKeys = stringsOnly(o.Get("cachedKeys"))
	probe, probeSeen := o.Lookup("probeEvidence")
	m.Probe, m.probeSeen = parseSelfHealProbeEvidence(probe), probeSeen
	return m
}

// ReadSelfHealMarkerFile distinguishes a cache miss from a file that cannot be read.
// Opt-out/clear must not mistake the latter for an empty consent record.
func ReadSelfHealMarkerFile(home string) (*SelfHealMarker, error) {
	raw, err := readTextOrNull(SelfHealMarkerPath(home))
	if err != nil || raw == nil {
		return nil, err
	}
	return ParseSelfHealMarker(*raw), nil
}

func selfHealMarkerObject(m *SelfHealMarker) (pyjson.Object, error) {
	fields := pyjson.Object{}
	add := func(key string, value any) { fields = append(fields, pyjson.Field{Key: key, Value: value}) }
	if m.OptedOut != nil {
		add("optedOut", *m.OptedOut)
	}
	if m.OptedOutAt != nil {
		add("optedOutAt", *m.OptedOutAt)
	}
	if m.ConfigMtimeMs != nil {
		var value any
		if n := *m.ConfigMtimeMs; !math.IsNaN(n) && !math.IsInf(n, 0) {
			if n == 0 {
				n = 0
			}
			b, err := role.Stringify(n, "")
			if err != nil {
				return nil, err
			}
			value = json.Number(b)
		}
		add("configMtimeMs", value)
	}
	if m.CheckedAt != nil {
		add("checkedAt", *m.CheckedAt)
	}
	if m.AllEnabled != nil {
		add("allEnabled", *m.AllEnabled)
	}
	if m.HealedKeys != nil {
		add("healedKeys", m.HealedKeys)
	}
	if m.CachedKeys != nil {
		add("cachedKeys", m.CachedKeys)
	}
	if m.Probe != nil {
		add("probeEvidence", selfHealProbeEvidenceObject(m.Probe))
	}
	ordered := pyjson.Object{}
	for _, key := range m.order {
		if value, ok := fields.Lookup(key); ok {
			ordered = append(ordered, pyjson.Field{Key: key, Value: value})
		}
	}
	for _, field := range fields {
		if !slices.Contains(m.order, field.Key) {
			ordered = append(ordered, field)
		}
	}
	return ordered, nil
}

// WriteSelfHealMarkerFile publishes through the same protected, fsynced temporary
// file and rename as activation. It never reuses another writer's fixed .tmp path.
func WriteSelfHealMarkerFile(home string, marker *SelfHealMarker) error {
	if marker.Probe != nil || marker.probeSeen {
		selfHealRetireLegacyCache(marker)
	}
	fields, err := selfHealMarkerObject(marker)
	if err != nil {
		return err
	}
	b, err := pyjson.Encode(fields, pyjson.Options{Indent: 2, Unicode: true})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0777); err != nil {
		return err
	}
	return activationPublish(SelfHealMarkerPath(home), append(b, '\n'))
}

// MarkSelfHealOptedOut merges the explicit disable choice with accepted cache and
// healed-key consent fields. Newly present overrides append in JS spread order.
func MarkSelfHealOptedOut(home, at string) error {
	m, err := ReadSelfHealMarkerFile(home)
	if err != nil {
		return err
	}
	if m == nil {
		m = &SelfHealMarker{}
	}
	fields, err := selfHealMarkerObject(m)
	if err != nil {
		return err
	}
	for _, field := range fields {
		m.order = append(m.order, field.Key)
	}
	for _, key := range []string{"optedOut", "optedOutAt", "allEnabled"} {
		if !slices.Contains(m.order, key) {
			m.order = append(m.order, key)
		}
	}
	opted, enabled := true, false
	m.OptedOut, m.OptedOutAt, m.AllEnabled = &opted, &at, &enabled
	// The flags this opt-out reverts no longer match what the evidence verified.
	m.Probe = nil
	return WriteSelfHealMarkerFile(home, m)
}

// ClearSelfHealOptOut clears only the explicit opt-out fields; an absent or
// readable malformed marker stays untouched, and a read failure is refused.
func ClearSelfHealOptOut(home string) error {
	m, err := ReadSelfHealMarkerFile(home)
	if err != nil || m == nil {
		return err
	}
	m.OptedOut, m.OptedOutAt = nil, nil
	return WriteSelfHealMarkerFile(home, m)
}
