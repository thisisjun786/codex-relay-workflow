package integrator

import (
	"crypto/sha1" //nolint:gosec // names a retry marker, as the shell runner's sha1sum did
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The queue's directories and files below Config.QueueDir.
const (
	dirPending  = "pending"
	dirDone     = "done"
	dirConflict = "conflict"
	dirBlocked  = "blocked"
	dirRetried  = "retried"
	fileStop    = "stop"
	fileLog     = "runner.log"
	fileSolo    = "solo-left"
)

var (
	headRE      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	queueNameRE = regexp.MustCompile(`^[0-9]{1,18}$`)
)

// Entry is one pending file: the issue number it is named after and the head it holds.
type Entry struct {
	Name   string `json:"name"`
	Key    string `json:"key"`
	Node   string `json:"node"`
	Head   string `json:"head"`
	HeadOK bool   `json:"head_ok"`
	num    uint64
}

// Spec is the "node=head" argument of the integration run.
func (e Entry) Spec() string { return e.Node + "=" + e.Head }

func (r *Runner) qpath(parts ...string) string {
	return filepath.Join(append([]string{r.Cfg.QueueDir}, parts...)...)
}

// ensureQueueDirs makes the queue's directories, as the runner's mkdir -p did.
func (r *Runner) ensureQueueDirs() error {
	for _, d := range []string{dirPending, dirDone, dirConflict, dirBlocked} {
		if err := os.MkdirAll(r.qpath(d), 0o775); err != nil {
			return err
		}
	}
	return nil
}

// readHead is the head a pending file holds: its content without spaces and newlines.
func readHead(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", errors.New("pending file is too large")
	}
	return strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(string(data)), nil
}

// listPending is the pending entries in issue-number order and the names it did not take (a
// directory, a link, or a name that is not an issue number).
func (r *Runner) listPending() (entries []Entry, ignored []string, err error) {
	des, err := os.ReadDir(r.qpath(dirPending))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, de := range des {
		name := de.Name()
		num, perr := strconv.ParseUint(name, 10, 64)
		if !queueNameRE.MatchString(name) || perr != nil || !de.Type().IsRegular() {
			ignored = append(ignored, name)
			continue
		}
		head, rerr := readHead(r.qpath(dirPending, name))
		if rerr != nil {
			ignored = append(ignored, name)
			continue
		}
		entries = append(entries, Entry{Name: name, Key: r.Cfg.key(name), Node: r.Cfg.node(name), Head: head, HeadOK: headRE.MatchString(head), num: num})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].num != entries[j].num {
			return entries[i].num < entries[j].num
		}
		return entries[i].Name < entries[j].Name
	})
	sort.Strings(ignored)
	return entries, ignored, nil
}

// pendingCount is how many regular files wait in pending.
func (r *Runner) pendingCount() int {
	entries, _, _ := r.listPending()
	return len(entries)
}

// moveEntry moves a pending file to one of the other queue directories, only while it still
// holds the head the run worked on: a coordinator that refreshed the head meanwhile keeps its
// new file in pending. It reports whether the file moved.
func (r *Runner) moveEntry(e Entry, to string) bool {
	src := r.qpath(dirPending, e.Name)
	if now, err := readHead(src); err != nil || now != e.Head {
		r.logf("%s %s changed or went away during the run: left in pending", e.Key, e.Head)
		return false
	}
	if err := os.MkdirAll(r.qpath(to), 0o775); err != nil {
		r.logf("%s: %s: %v", e.Key, to, err)
		return false
	}
	if err := os.Rename(src, r.qpath(to, e.Name)); err != nil {
		r.logf("%s: move to %s: %v", e.Key, to, err)
		return false
	}
	return true
}

// batchKey names a batch for its retry marker: the first 16 hex characters of the sha1 of the
// specs, one per line (the shell runner's printf '%s\n' | sha1sum | cut -c1-16).
func batchKey(entries []Entry) string {
	h := sha1.New() //nolint:gosec // a marker name, not a security property
	for _, e := range entries {
		_, _ = h.Write([]byte(e.Spec() + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (r *Runner) batchMarker(entries []Entry) string {
	return r.qpath(dirRetried, "batch-"+batchKey(entries))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// touch makes an empty marker file.
func (r *Runner) touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o664)
}

// soloLeft is how many more single-head runs the failed batch still owes (0: batching).
func (r *Runner) soloLeft() int {
	data, err := os.ReadFile(r.qpath(fileSolo))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (r *Runner) setSoloLeft(n int) {
	path := r.qpath(fileSolo)
	if n <= 0 {
		_ = os.Remove(path)
		return
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", n)), 0o664); err != nil {
		r.logf("solo state: %v", err)
	}
}
