package staging

import "strings"

// TombstonePrefix names a runtime directory crw install is removing: remove, the reclaim of an
// abandoned staging and the release of a failed candidate first rename the directory to
// <destination>/.crw-removing-<name> in one atomic step, then delete that. A kill part-way
// through the deletion leaves the tombstone, never a directory under the runtime's own name
// with some of its files (its claim among them) gone. crw install remove finishes one; status
// lists every one, and the residue survey lists the ones whose claim is an abandoned staging's.
const TombstonePrefix = ".crw-removing-"

// RuntimeDirectory is whether name is one of the installer's runtime directories,
// bin-<version>-<digest>.
func RuntimeDirectory(name string) bool {
	return strings.HasPrefix(name, "bin-")
}

// TombstoneOf is the runtime directory name a tombstone name was renamed from. A name that only
// carries the prefix is not a tombstone: crw install remove refuses it, so nothing may point
// at it as the recovery.
func TombstoneOf(name string) (string, bool) {
	original, ok := strings.CutPrefix(name, TombstonePrefix)
	return original, ok && RuntimeDirectory(original)
}

// RemovalRecovery is the recovery for the tombstone at path, whichever command reports it
// (crw install status, the residue survey of crw doctor). An install reclaims the directory
// under a runtime's own name and never a tombstone for its own sake, so crw install remove is
// what finishes one.
func RemovalRecovery(path string) string {
	return "crw install remove " + path + " finishes this removal, once no process runs out of it and no registration names it. Do not delete it by hand: finishing it also drops what the host record still lists under the runtime's name"
}
