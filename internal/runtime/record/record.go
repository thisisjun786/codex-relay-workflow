// Package record is scripts/crw_runtime/hostrecord.py: the private host record at
// ${XDG_STATE_HOME:-~/.local/state}/codex-relay-workflow/host-record.json, what is installed
// here and what was exercised here.
//
// The record stays recordVersion 1 and is written byte for byte as Python writes it
// (json.dump(indent=2, sort_keys=True, ensure_ascii=True) plus a newline), because the Python
// installer, the developer harness and both fault sweepers (faultsweep.py and
// internal/relay/faults/sweep.go) read the same file during coexistence and refuse any other
// version. A Go install entry is additive: it carries the keys the sweepers read (location,
// environment, integrity, source) plus binaryDigest and target, and omits the interpreter
// fields. Python entries and points are carried through untouched.
package record

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
)

// Name is the record's file name.
const Name = "host-record.json"

// Version is the only recordVersion any reader accepts.
const Version = 1

// Object is a decoded JSON object in Python's insertion order.
type Object = contract.OrderedObject

// Empty is hostrecord.empty: the record a clean host starts from.
func Empty(definitionVersion int) Object {
	host, _ := os.Hostname()
	return Object{
		{Key: "recordVersion", Value: int64(Version)},
		{Key: "definitionVersion", Value: int64(definitionVersion)},
		{Key: "host", Value: host},
		{Key: "user", Value: os.Getenv("USER")},
		{Key: "components", Value: Object{}},
	}
}

// Shape is hostrecord.shape: reject a record whose containers are not what every consumer
// assumes. Present-and-null is not absence.
func Shape(v any) error {
	record, ok := v.(Object)
	if !ok {
		return reading.Fail("TypeError", "a host record is an object, found "+reading.JSONKind(v))
	}
	components, has := Lookup(record, "components")
	if has {
		if _, ok := components.(Object); !ok {
			return reading.Fail("TypeError", "components is an object, found "+reading.JSONKind(components))
		}
	}
	all, _ := components.(Object)
	for _, c := range all {
		entry, ok := c.Value.(Object)
		if !ok {
			return reading.Fail("TypeError", "component "+c.Key+" is an object, found "+reading.JSONKind(c.Value))
		}
		for _, key := range []string{"installs", "measuredPoints"} {
			value, has := Lookup(entry, key)
			if !has {
				continue
			}
			items, ok := value.([]any)
			if !ok {
				return reading.Fail("TypeError", c.Key+"."+key+" is a list, found "+reading.JSONKind(value))
			}
			for _, item := range items {
				if _, ok := item.(Object); !ok {
					return reading.Fail("TypeError", "every entry in "+c.Key+"."+key+" is an object, found "+reading.JSONKind(item))
				}
			}
		}
	}
	if selected := Get(record, "selected"); selected != nil {
		if _, ok := selected.(Object); !ok {
			return reading.Fail("TypeError", "selected is an object, found "+reading.JSONKind(selected))
		}
	}
	if owned := Get(record, "pointer"); owned != nil {
		entry, ok := owned.(Object)
		if !ok {
			return reading.Fail("TypeError", "pointer is an object, found "+reading.JSONKind(owned))
		}
		if path, has := Lookup(entry, "path"); has {
			if _, ok := path.(string); !ok {
				return reading.Fail("TypeError", "pointer.path is a string, found "+reading.JSONKind(path))
			}
		}
	}
	return nil
}

// Load reads the record as a reading, never as a sentinel. An unreadable record is never
// replaced silently.
func Load(path string, definitionVersion int) reading.Reading {
	return reading.ReadJSON(path, "the host record", func() any { return Empty(definitionVersion) }, Shape)
}

// Encode is json.dump(record, indent=2, sort_keys=True) followed by the newline save writes.
func Encode(value any) []byte {
	return []byte(pyjson.Dumps(value, pyjson.Options{Indent: 2, SortKeys: true}) + "\n")
}

// Save is hostrecord.save: atomic, so an interrupted write cannot leave a truncated record.
func Save(path string, value any) error {
	return writeAtomic(path, ".host-record-", Encode(value))
}

// AtomicWrite is hostrecord.atomic_write: temp file beside the target, then rename, which
// replaces a symlink at path rather than following it.
func AtomicWrite(path string, text []byte) error {
	return writeAtomic(path, ".crw-write-", text)
}

func writeAtomic(path, prefix string, data []byte) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), prefix)
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(temporary)
		}
	}()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Lookup is dict lookup.
func Lookup(o Object, key string) (any, bool) {
	for _, f := range o {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// Get is dict.get(key).
func Get(o Object, key string) any {
	v, _ := Lookup(o, key)
	return v
}

// Set replaces key's value in place, or appends it (dict assignment).
func Set(o Object, key string, value any) Object {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = value
			return o
		}
	}
	return append(o, contract.Field{Key: key, Value: value})
}

// Delete is del o[key].
func Delete(o Object, key string) Object {
	out := o[:0:0]
	for _, f := range o {
		if f.Key != key {
			out = append(out, f)
		}
	}
	return out
}

// Text is the string at key, or "".
func Text(o Object, key string) string {
	s, _ := Get(o, key).(string)
	return s
}

// Component is hostrecord.component: the component entry with its two lists present. It
// returns the record (which may have grown) and the entry.
func Component(record Object, name string) (Object, Object) {
	components, _ := Get(record, "components").(Object)
	entry, _ := Get(components, name).(Object)
	if entry == nil {
		entry = Object{}
	}
	if _, ok := Lookup(entry, "installs"); !ok {
		entry = Set(entry, "installs", []any{})
	}
	if _, ok := Lookup(entry, "measuredPoints"); !ok {
		entry = Set(entry, "measuredPoints", []any{})
	}
	components = Set(components, name, entry)
	return Set(record, "components", components), entry
}

// withComponent writes entry back as component name.
func withComponent(record Object, name string, entry Object) Object {
	components, _ := Get(record, "components").(Object)
	return Set(record, "components", Set(components, name, entry))
}

// PutInstall is hostrecord.put_install: replace the entry for this location, or add it.
func PutInstall(record Object, name string, install Object) Object {
	record, entry := Component(record, name)
	location := Get(install, "location")
	kept := []any{}
	for _, item := range Get(entry, "installs").([]any) {
		if !equalJSON(Get(item.(Object), "location"), location) {
			kept = append(kept, item)
		}
	}
	entry = Set(entry, "installs", append(kept, install))
	return withComponent(record, name, entry)
}

// AddPoint is hostrecord.add_point: append, never replace.
func AddPoint(record Object, name string, point Object) Object {
	record, entry := Component(record, name)
	entry = Set(entry, "measuredPoints", append(Get(entry, "measuredPoints").([]any), point))
	return withComponent(record, name, entry)
}

func equalJSON(a, b any) bool {
	return pyjson.Dumps(a, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) == pyjson.Dumps(b, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
}

// Presence policies of a point dimension.
const (
	Mandatory = "mandatory"
	Symmetric = "symmetric"
)

// Dimension is one field a Go point is compared on.
type Dimension struct{ Field, Policy string }

// Dimensions is the Go install's dimension set: hostrecord.DIMENSIONS without interpreter and
// exerciseDigest, which a single binary has no counterpart for. installDigest is the binary's
// SHA-256.
var Dimensions = []Dimension{
	{"install", Mandatory},
	{"installDigest", Mandatory},
	{"codexCli", Mandatory},
	{"host", Mandatory},
	{"appServer", Symmetric},
}

// PointsFor is hostrecord.points_for over Dimensions: points that actually cover this install
// and these bytes. wanted maps a dimension field to the caller's value; an absent key is None.
func PointsFor(record Object, name string, wanted map[string]string) []Object {
	_, entry := Component(record, name)
	var found []Object
	for _, raw := range Get(entry, "measuredPoints").([]any) {
		point, _ := raw.(Object)
		if !pyvalue.Truthy(Get(point, "exercised")) {
			continue
		}
		if Get(point, "digestMatchesDefinition") == false {
			continue
		}
		covers := true
		for _, d := range Dimensions {
			recorded, has := Lookup(point, d.Field)
			if recorded == nil {
				has = false
			}
			want, asked := wanted[d.Field]
			if !has && !asked {
				if d.Policy == Mandatory {
					covers = false
					break
				}
				continue
			}
			if !has || !asked || !equalJSON(recorded, want) {
				covers = false
				break
			}
		}
		if covers {
			found = append(found, point)
		}
	}
	return found
}

// Stated is hostrecord.stated: a string with something in it.
func Stated(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

// PointerPlacement are the keys that record a link THIS COMMAND placed at the pointer path.
var PointerPlacement = []string{"recordedAt", "recordedBy"}

// PlacementRecorded is hostrecord.placement_recorded: non-blank strings, not truthiness.
func PlacementRecorded(entry any) bool {
	o, ok := entry.(Object)
	if !ok || !Stated(Get(o, "path")) {
		return false
	}
	for _, key := range PointerPlacement {
		if !Stated(Get(o, key)) {
			return false
		}
	}
	return true
}

// WithoutPlacement is the same entry with its placement evidence withdrawn and its path kept.
func WithoutPlacement(entry Object) Object {
	out := Object{}
	for _, f := range entry {
		if f.Key != "recordedAt" && f.Key != "recordedBy" {
			out = append(out, f)
		}
	}
	return out
}

// Restore is the compare-and-replace pointer delta: Wrote is the path this run recorded and
// the only one it may compare against; Found is the entry it replaced and what goes back.
type Restore struct {
	Wrote string
	Found Object
}

// Named is one (component, entry) pair of a delta.
type Named struct {
	Component string
	Entry     Object
}

// Outgoing replaces the record's outgoing selection: the selection a promotion replaced, which
// `crw install rollback` returns to. A nil Value removes the key.
type Outgoing struct{ Value Object }

// Delta is what a caller learned, merged by Update into the record as it stands under the
// lock. The retired Python delta component_facts is not carried; outgoing is written by the
// promotion that replaces a selection, in the same write that commits the new one.
type Delta struct {
	Installs        []Named
	Points          []Named
	Select          []contract.Field // component -> location, only the assignments this run made
	DropEnvironment *string
	Pointer         Object
	Deselect        []contract.Field // component -> location this run wrote
	DropPointer     *string
	RestorePointer  *Restore
	Outgoing        *Outgoing
}

// Update is hostrecord.update: apply narrow deltas to state this helper loads itself, inside
// the .crw-lock, at write time. Nothing is written when the record could not be read; the
// returned reading says why. A lock that could not be taken is a *Busy error.
func Update(path string, definitionVersion int, delta Delta) (reading.Reading, error) {
	return UpdateContext(context.Background(), path, definitionVersion, delta)
}

// UpdateContext is Update whose wait for the record's lock ends, writing nothing, once ctx is
// done.
func UpdateContext(ctx context.Context, path string, definitionVersion int, delta Delta) (reading.Reading, error) {
	lock, err := LockContext(ctx, path, 0)
	if err != nil {
		return reading.Reading{}, err
	}
	defer lock.Release()
	current := Load(path, definitionVersion)
	if !current.Usable() {
		return current, nil
	}
	record := current.Value.(Object)
	for _, one := range delta.Installs {
		record = PutInstall(record, one.Component, one.Entry)
	}
	for _, one := range delta.Points {
		record = AddPoint(record, one.Component, one.Entry)
	}
	if delta.DropEnvironment != nil {
		record = dropEnvironment(record, *delta.DropEnvironment)
	}
	if delta.Pointer != nil {
		owned, _ := Get(record, "pointer").(Object)
		for _, f := range delta.Pointer {
			owned = Set(owned, f.Key, f.Value)
		}
		if owned == nil {
			owned = Object{}
		}
		record = Set(record, "pointer", owned)
	}
	if len(delta.Select) > 0 {
		selected, _ := Get(record, "selected").(Object)
		for _, f := range delta.Select {
			selected = Set(selected, f.Key, f.Value)
		}
		record = Set(record, "selected", selected)
	}
	for _, f := range delta.Deselect {
		if selected, ok := Get(record, "selected").(Object); ok && equalJSON(Get(selected, f.Key), f.Value) {
			if _, has := Lookup(selected, f.Key); has {
				record = Set(record, "selected", Delete(selected, f.Key))
			}
		}
	}
	if delta.DropPointer != nil {
		if owned, ok := Get(record, "pointer").(Object); ok && Get(owned, "path") == *delta.DropPointer {
			record = Delete(record, "pointer")
		}
	}
	if delta.RestorePointer != nil {
		if owned, ok := Get(record, "pointer").(Object); ok && Get(owned, "path") == delta.RestorePointer.Wrote {
			record = Set(record, "pointer", append(Object{}, delta.RestorePointer.Found...))
		}
	}
	if delta.Outgoing != nil {
		if delta.Outgoing.Value == nil {
			record = Delete(record, "outgoing")
		} else {
			record = Set(record, "outgoing", delta.Outgoing.Value)
		}
	}
	current.Value = record
	if err := Save(path, record); err != nil {
		return current, err
	}
	return current, nil
}

func dropEnvironment(record Object, environment string) Object {
	components, _ := Get(record, "components").(Object)
	for i, c := range components {
		entry, _ := c.Value.(Object)
		installs, _ := Get(entry, "installs").([]any)
		kept := []any{}
		for _, item := range installs {
			if Get(item.(Object), "environment") != environment {
				kept = append(kept, item)
			}
		}
		components[i].Value = Set(entry, "installs", kept)
	}
	return record
}

// ReleaseCandidate is hostrecord.release_candidate: drop the install records for an
// environment unless it is selected or the pointer may still name it. pointerNames is the
// caller's pointer reading: true, false, or nil when it could not be established.
func ReleaseCandidate(path string, definitionVersion int, environment string, pointerNames *bool) (reading.Reading, string) {
	lock, err := Lock(path, 0)
	if err != nil {
		var busy *Busy
		if errors.As(err, &busy) {
			return reading.Reading{State: reading.AccessError, Source: path, Exception: "Busy", Detail: "the host record lock could not be taken: " + err.Error()},
				"kept: the host record lock could not be taken, so nothing about the selection could be established"
		}
		return reading.Reading{State: reading.AccessError, Source: path, Exception: "OSError", Detail: "the host record lock could not be taken: " + err.Error()},
			"kept: the host record lock could not be taken, so nothing about the selection could be established"
	}
	defer lock.Release()
	current := Load(path, definitionVersion)
	if !current.Usable() {
		return current, "kept: the record could not be read, so nothing about the selection could be established"
	}
	record := current.Value.(Object)
	if selected, ok := Get(record, "selected").(Object); ok {
		for _, f := range selected {
			if location, ok := f.Value.(string); ok && location != "" && Under(location, environment) {
				return current, "kept: this environment is the selected one, so the run that promoted it committed before it failed"
			}
		}
	}
	if pointerNames == nil || *pointerNames {
		if pointerNames != nil {
			return current, "kept: the owned pointer names this environment, so the command a host reaches still resolves into it"
		}
		return current, "kept: whether the owned pointer names this environment could not be established, and an unread pointer is not a pointer aimed elsewhere"
	}
	record = dropEnvironment(record, environment)
	current.Value = record
	if err := Save(path, record); err != nil {
		return current, "kept: the record could not be written: " + err.Error()
	}
	return current, "dropped: the selection does not name this environment"
}

// Under is hostrecord._under: containment over resolved parts, never a string prefix.
func Under(location, environment string) bool {
	candidate, err := Resolve(location)
	if err != nil {
		return false
	}
	root, err := Resolve(environment)
	if err != nil {
		return false
	}
	return Within(candidate, root)
}

// Within is runtime_install.within: whether a resolved path is root or lies under it.
func Within(candidate, root string) bool {
	if candidate == root {
		return true
	}
	return strings.HasPrefix(candidate, strings.TrimSuffix(root, "/")+"/")
}

// Resolve is Path.resolve() (os.path.realpath, strict=False): symlinks followed, a missing
// tail kept as spelled.
func Resolve(path string) (string, error) {
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("embedded null byte")
	}
	return resolvePath(path)
}
