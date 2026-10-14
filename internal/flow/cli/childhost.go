package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The host settings of a managed child start (SP-CHILD-1, CRW-1285). A node the architect sized for a child is released as an issue
// child: dag-release's managed start of a task on the run's host, which needs the settings the parent and the child run with, the
// child's title and the marker root the start is fingerprinted with. The run holds them as it holds its handoff: `crw flow start
// --child-host FILE` checks the crw-flow-child-host/1 document, pins its bytes into the run's directory and names the copy and its
// sha256 in the run's arguments ("child_host"); every process of the run reads the pinned copy. A run started without one starts no
// child: a node sized for a child is refused with the reason (an input error), never answered as a child that is not running.

// ChildHostSchema is the schema of the document.
const ChildHostSchema = "crw-flow-child-host/1"

// ChildHostFile is the name of a run's pinned copy of it.
const ChildHostFile = "child-host.json"

// ChildHost is the document: the marker root of the managed start, and the parent's and the child's endpoint (dag-release-request/1
// parent and child: the host, the settings each task runs with, the child's title).
type ChildHost struct {
	Schema     string            `json:"schema"`
	MarkerRoot string            `json:"marker_root"`
	Parent     dagsched.Endpoint `json:"parent"`
	Child      dagsched.Endpoint `json:"child"`
}

// ErrChildHostUnset is the refusal of a node sized for a child in a run that holds no host settings.
var ErrChildHostUnset = errors.New("refused:child_host_unset: the run holds no host settings for a managed child start (SP-CHILD-1): start the run with --child-host FILE, a " +
	ChildHostSchema + " document naming the parent's and the child's host, settings and the child's title")

// Problem says why the document cannot start a managed child ("" when it can): the schema, an absolute marker root, the hosts, the
// settings and the child's title, one host for both (a managed start supports one local host). The settings themselves are judged by the
// managed start's own reading of the request (managed.ParseRequest), when a release assembles it.
func (h ChildHost) Problem() string {
	var missing []string
	if h.Schema != ChildHostSchema {
		return fmt.Sprintf("the document's schema is %q, not %s", h.Schema, ChildHostSchema)
	}
	if !filepath.IsAbs(h.MarkerRoot) {
		missing = append(missing, "marker_root (an absolute path)")
	}
	for _, f := range []struct {
		name  string
		empty bool
	}{{"parent.host_id", strings.TrimSpace(h.Parent.HostID) == ""}, {"parent.settings", len(h.Parent.Settings) == 0},
		{"child.host_id", strings.TrimSpace(h.Child.HostID) == ""}, {"child.title", strings.TrimSpace(h.Child.Title) == ""}, {"child.settings", len(h.Child.Settings) == 0}} {
		if f.empty {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return "the child host settings name no " + strings.Join(missing, ", ")
	}
	if h.Parent.HostID != h.Child.HostID {
		return fmt.Sprintf("the parent's host %s is not the child's %s: a managed start supports one local host", h.Parent.HostID, h.Child.HostID)
	}
	return ""
}

// DecodeChildHost reads the document strictly (an unknown member or a second document is refused) and checks it (Problem).
func DecodeChildHost(raw []byte) (ChildHost, error) {
	var h ChildHost
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return ChildHost{}, fmt.Errorf("the child host settings are not a %s document: %w", ChildHostSchema, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ChildHost{}, fmt.Errorf("the child host settings hold more than one %s document", ChildHostSchema)
	}
	if why := h.Problem(); why != "" {
		return ChildHost{}, errors.New(why)
	}
	return h, nil
}

// ReadChildHost reads the pinned copy a reference names, checked against the sha256 the run fixed.
func ReadChildHost(ref HandoffRef) (ChildHost, error) {
	if !filepath.IsAbs(ref.Path) || !sha256Pattern.MatchString(ref.SHA256) {
		return ChildHost{}, fmt.Errorf("the child host reference %+v names no absolute file and sha256", ref)
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil {
		return ChildHost{}, fmt.Errorf("the child host settings cannot be read: %w", err)
	}
	if got := sha256Hex(raw); got != ref.SHA256 {
		return ChildHost{}, fmt.Errorf("the child host settings %s changed: their sha256 is %s and %s was fixed", ref.Path, got, ref.SHA256)
	}
	return DecodeChildHost(raw)
}

// PinChildHost writes the document's bytes into the run's directory, once, and answers the reference the run's arguments carry.
func PinChildHost(state, run string, raw []byte) (HandoffRef, error) {
	if !validRunID(run) {
		return HandoffRef{}, fmt.Errorf("%q is not a run id", run)
	}
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		return HandoffRef{}, err
	}
	path := filepath.Join(RunDir(state, run), ChildHostFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return HandoffRef{}, err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return HandoffRef{}, err
	}
	if err := f.Close(); err != nil {
		return HandoffRef{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return HandoffRef{}, err
	}
	return HandoffRef{Path: abs, SHA256: sha256Hex(raw)}, nil
}

// ChildHostOf is the child host settings a run's arguments name ("child_host").
func ChildHostOf(args json.RawMessage) (HandoffRef, bool) {
	var a struct {
		ChildHost *HandoffRef `json:"child_host"`
	}
	if json.Unmarshal(args, &a) != nil || a.ChildHost == nil {
		return HandoffRef{}, false
	}
	return *a.ChildHost, true
}

// withChildHost is a run's arguments with the child host reference added as their last member.
func withChildHost(args json.RawMessage, ref HandoffRef) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte("{}")
	}
	if json.Unmarshal(trimmed, &obj) != nil {
		return nil, errors.New("the workflow's args are not a JSON object, so they cannot name the child host settings")
	}
	if _, ok := obj["child_host"]; ok {
		return nil, errors.New("the args name child host settings of their own: the run's are given with --child-host")
	}
	member, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, trimmed[:len(trimmed)-1]...)
	if len(obj) > 0 {
		out = append(out, ',')
	}
	out = append(append(append(out, `"child_host":`...), member...), '}')
	return compactJSON(out), nil
}
