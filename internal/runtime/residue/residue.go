// Package residue is scripts/crw_runtime/residue.py: what a run left behind on an install
// destination, asked of the decision that owns it. An entry is residue exactly when the
// installer's own staging.Decide would reclaim it, so every guard that decision carries is
// inherited rather than restated. It writes nothing and removes nothing, and it holds no lock:
// a listed path is only safely clearable under the installer's lock.
package residue

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// Object is a decoded JSON object.
type Object = record.Object

// Why a path is named beside the staging decision itself.
const (
	DanglingPointer           = "dangling_pointer"
	ForeignPointer            = "foreign_pointer"
	UnreadablePointerTarget   = "unreadable_pointer_target"
	PointerOutsideDestination = "pointer_outside_destination"
	NotScanned                = "not_scanned"
)

// Note is residue.NOTE.
const Note = "residue is what the installer's own decision would reclaim (staging.Removes), so this is that decision rather than a second opinion about the same directory. It is not guaranteed to be the same SET as a later install's, and the difference is stated rather than implied: this command asks about the pointer the host RECORD names, and crw install install asks about the destination it acts on. Those are the same directory on an ordinary host and not on one whose recorded pointer lies elsewhere, where this command is the conservative of the two -- it protects an environment the recorded pointer still reaches. Which of the two questions the installer should ask is a decision about the installer, and it is not this issue's to make. A failed install's residualPaths is a different reading: it is what THAT RUN left, and this is what is on the destination now. It is also not a snapshot: the claim, the lock, the contents, the host record and the pointer are read at different moments, so a host changing underneath this command is described in pieces. Every decision is conservative in the same direction, so an error costs a path being kept rather than one being missed. What it does NOT promise is that a listed path is still residue when this payload is read: this survey takes no lock, so an install can reclaim and rebuild a path between the reading and the reading being acted on. Entries can go stale, and a path is only safely clearable under the installer lock -- which is why the recovery text names the install rather than a removal."

// Protection is the caller's ownership reading of one directory: protected (conservative) and
// selected (the record was read and names it).
type Protection func(directory string) (protected bool, selected *bool)

func entry(path string, fields ...record.Object) Object {
	out := Object{{Key: "path", Value: path}, {Key: "decision", Value: nil}, {Key: "reason", Value: nil}, {Key: "residual", Value: false}}
	for _, more := range fields {
		for _, f := range more {
			out = record.Set(out, f.Key, f.Value)
		}
	}
	return out
}

func strs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Survey is residue.survey: every directory under destination, classified by staging.Decide.
// A nil protection means no ownership reading was made, and then nothing is reported as
// residue: no residue found and nobody looked are different answers. unreadable carries the
// readings the caller already failed to take about this destination.
func Survey(destination *string, pointerPath string, pointerOwnership any, protection Protection, unreadable []string) Object {
	var entries []any
	residual, recovery := []string{}, []string{}
	unread := append([]string{}, unreadable...)
	answer := Object{{Key: "destination", Value: nil}, {Key: "read", Value: false}}
	if destination == nil {
		if len(unread) == 0 {
			unread = append(unread, "no destination was named, so nothing was scanned")
		}
		return finish(answer, entries, residual, recovery, unread, protection != nil, nil)
	}
	root := *destination
	answer = record.Set(answer, "destination", root)
	excluded := ""
	if pointerPath != "" && reading.SameDirectory(filepath.Dir(pointerPath), root) {
		excluded = filepath.Join(root, filepath.Base(pointerPath))
	}
	listed, err := os.ReadDir(root)
	if err != nil || strings.ContainsRune(root, 0) {
		said := "ValueError: embedded null byte"
		if err != nil {
			said = store.PythonOSError(err)
		}
		unread = append(unread, "the destination could not be listed: "+said)
		finding := pointerFinding(pointerPath, pointerOwnership, root)
		return finish(answer, entries, residual, recovery, unread, protection != nil, finding)
	}
	answer = record.Set(answer, "read", true)
	names := make([]string, 0, len(listed))
	for _, e := range listed {
		names = append(names, filepath.Join(root, e.Name()))
	}
	sort.Strings(names)
	for _, path := range names {
		if path == excluded {
			entries = append(entries, entry(path, Object{{Key: "decision", Value: NotScanned}, {Key: "reason", Value: "this is the owned pointer, not an environment under this destination"}}))
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			detail := "this entry could not be inspected: " + store.PythonOSError(err)
			entries = append(entries, entry(path, Object{{Key: "decision", Value: NotScanned}, {Key: "reason", Value: detail}}))
			unread = append(unread, path+": "+detail)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			entries = append(entries, entry(path, Object{{Key: "decision", Value: NotScanned}, {Key: "reason", Value: "a symbolic link is not an environment this command built, and it is not followed"}}))
			continue
		}
		if !info.IsDir() {
			entries = append(entries, entry(path, Object{{Key: "decision", Value: NotScanned}, {Key: "reason", Value: "not a directory"}}))
			continue
		}
		claim := staging.ReadClaim(path)
		liveness, livenessDetail := staging.OwnerLiveness(path)
		occupied, occupiedDetail := staging.DirectoryOccupied(path)
		if protection == nil {
			entries = append(entries, entry(path, Object{{Key: "decision", Value: NotScanned}, {Key: "claimState", Value: claim.State}, {Key: "liveness", Value: liveness},
				{Key: "reason", Value: "no ownership reading was supplied, so whether anything selects or reaches this environment was not established and nothing about it is reported as clearable"}}))
			continue
		}
		protected, selected := protection(path)
		decision, reason := staging.Decide(claim, liveness, occupied, protected, selected != nil && *selected)
		isResidual := staging.Removes(decision)
		var claimReading any
		if !claim.Usable() {
			claimReading = claim.Refusal()
		}
		var occupiedValue, selectedValue any
		if occupied != nil {
			occupiedValue = *occupied
		}
		if selected != nil {
			selectedValue = *selected
		}
		entries = append(entries, entry(path, Object{
			{Key: "decision", Value: decision}, {Key: "reason", Value: reason}, {Key: "residual", Value: isResidual},
			{Key: "claimState", Value: claim.State}, {Key: "claimReading", Value: claimReading},
			{Key: "liveness", Value: liveness}, {Key: "livenessDetail", Value: livenessDetail},
			{Key: "occupied", Value: occupiedValue}, {Key: "occupiedDetail", Value: occupiedDetail},
			{Key: "recordSelectsIt", Value: selectedValue}, {Key: "protected", Value: protected},
		}))
		switch {
		case isResidual:
			residual = append(residual, path)
			recovery = append(recovery, "let the next install of this same combination reclaim "+path+", which takes the lock this reading did not: "+reason+". Removing it by hand means re-reading it first, because this survey holds no lock and an install may have started building there since it looked")
		case !claim.Usable():
			unread = append(unread, path+": "+claim.Detail)
		case liveness == staging.Unknown:
			unread = append(unread, path+": "+livenessDetail)
		}
	}
	return finish(answer, entries, residual, recovery, unread, protection != nil, pointerFinding(pointerPath, pointerOwnership, root))
}

func finish(answer Object, entries []any, residual, recovery, unread []string, ownershipRead bool, finding Object) Object {
	if finding != nil && record.Get(finding, "residual") == true {
		path, _ := record.Get(finding, "path").(string)
		residual = append(residual, path)
		recovery = append(recovery, "the pointer at "+path+" names a target that is not there, and the host record records it as this command's own, so nothing reaches a runtime through it until an install repoints it. Repointing is the recovery, and an install does it under the lock this reading did not hold. This command does NOT recommend removing the link by hand: rereading it first does not close the gap, because a run can repoint it between the reread and the removal, and taking away a link that has become live breaks every registered command that goes through it. pointer.remove exists for exactly that reason -- it refuses a link that has stopped naming what its caller placed")
	}
	if entries == nil {
		entries = []any{}
	}
	var pointerValue any
	if finding != nil {
		pointerValue = finding
	}
	return append(answer, record.Object{
		{Key: "entries", Value: entries},
		{Key: "pointer", Value: pointerValue},
		{Key: "residualPaths", Value: strs(residual)},
		{Key: "recoveryRequires", Value: strs(recovery)},
		{Key: "unreadable", Value: strs(unread)},
		{Key: "ownershipRead", Value: ownershipRead},
		{Key: "note", Value: Note},
	}...)
}

// pointerFinding is residue._pointer_finding: a dangling pointer is residue only when the host
// record positively records THIS path as a link this command placed, and only when it sits in
// the surveyed destination.
func pointerFinding(pointerPath string, ownership any, destination string) Object {
	if pointerPath == "" {
		return Object{{Key: "path", Value: nil}, {Key: "finding", Value: NotScanned}, {Key: "residual", Value: false}, {Key: "detail", Value: "no pointer was named to read"}}
	}
	path := store.PathlibSpelling(pointerPath)
	if !reading.SameDirectory(filepath.Dir(path), destination) {
		return Object{{Key: "path", Value: path}, {Key: "finding", Value: PointerOutsideDestination}, {Key: "residual", Value: false}, {Key: "destination", Value: destination},
			{Key: "detail", Value: "this pointer sits under " + filepath.Dir(path) + " and this survey describes " + destination + ", so it belongs to another installation and nothing about it is reported here"}}
	}
	read := pointer.Read(path)
	claimed := false
	if record.PlacementRecorded(ownership) {
		recorded, _ := record.Get(ownership.(Object), "path").(string)
		claimed = store.PathlibSpelling(recorded) == path
	}
	var target any
	if read.State == pointer.Link {
		target = read.Target
	}
	found := Object{{Key: "path", Value: path}, {Key: "state", Value: read.State}, {Key: "target", Value: target}, {Key: "detail", Value: read.Detail}, {Key: "recordClaimsIt", Value: claimed}, {Key: "residual", Value: false}, {Key: "finding", Value: nil}}
	if read.State != pointer.Link {
		return found
	}
	named := read.Target
	if !filepath.IsAbs(named) {
		named = filepath.Dir(path) + "/" + named
	}
	_, err := os.Stat(named)
	switch {
	case err == nil:
		return found
	case !errors.Is(err, os.ErrNotExist):
		found = record.Set(found, "finding", UnreadablePointerTarget)
		return record.Set(found, "detail", "whether the target exists could not be established: "+store.PythonOSError(err)+", so whether this pointer still reaches a runtime was not established and it is reported rather than listed for removal")
	}
	if claimed {
		found = record.Set(found, "finding", DanglingPointer)
		found = record.Set(found, "residual", true)
		return record.Set(found, "detail", "the pointer names "+read.Target+", which does not exist, and the host record records this path as this command's own pointer")
	}
	found = record.Set(found, "finding", ForeignPointer)
	return record.Set(found, "detail", "the pointer names "+read.Target+", which does not exist. The host record does not record a link THIS COMMAND PLACED at this path -- either it names another path, or a rollback established the link it placed is gone and withdrew the placement while keeping the path it is known by -- so whose link this is was not established and it is reported rather than listed for removal")
}
