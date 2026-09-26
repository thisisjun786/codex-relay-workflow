package store

import (
	"fmt"
	"strconv"
	"strings"
)

type StoreVerdict string

const (
	Proven   StoreVerdict = "proven"
	Unproven StoreVerdict = "unproven"
	Mismatch StoreVerdict = "mismatch"
)

// CompareExpectations are what another participant reported about its store. An empty string
// or a nil nonce means that expectation was not supplied, which is not agreement.
type CompareExpectations struct {
	StoreID string
	Inode   string
	Log     string
	Nonce   *NonceReading
	// The *Given flags mark an expectation supplied as the empty string, which Python's
	// `is not None` still counts as asked and grades like any other unusable value.
	StoreIDGiven, InodeGiven, LogGiven bool
}

type StoreComparison struct {
	SameStore StoreVerdict
	Detail    string
}

type gradedReason struct {
	verdict StoreVerdict // "" is agreement that proves nothing
	detail  string
}

// tristate is Python's None/True/False for "not compared", "agreed", "disagreed".
type tristate int

const (
	notCompared tristate = iota
	agreed
	disagreed
)

// CompareStore is store.compare_store: conflicting evidence is decided before agreeing
// evidence, and proof takes a found, attributed nonce AND an agreeing device and inode AND an
// agreeing log location. A zero device, inode or link count is "could not be measured".
func CompareStore(loc Location, expected CompareExpectations) StoreComparison {
	var reasons []gradedReason
	add := func(verdict StoreVerdict, detail string) { reasons = append(reasons, gradedReason{verdict, detail}) }
	if expected.StoreID != "" || expected.StoreIDGiven {
		switch {
		case loc.StoreID == "":
			add(Unproven, "this store states no identity, so it cannot be compared")
		case loc.StoreID != expected.StoreID:
			add(Mismatch, fmt.Sprintf("store id %s is not %s", loc.StoreID, expected.StoreID))
		default:
			add("", "store id matches")
		}
	}
	physical := comparePhysical(loc, expected.Inode, expected.InodeGiven, add)
	logLocation := compareLog(loc, expected.Log, expected.LogGiven, add)
	if n := expected.Nonce; n != nil {
		gradeNonce(loc, *n, physical, logLocation, add)
	}
	if len(reasons) > 0 {
		counted := uint64(0)
		for _, count := range []uint64{loc.Links, nonceLinks(expected.Nonce)} {
			counted = max(counted, count)
		}
		switch {
		case counted == 0:
			add(Unproven, "the number of names this database has could not be measured")
		case counted > 1:
			add(Unproven, fmt.Sprintf("this database has %d names, so a shared device and inode cannot say which one the other participant opened, and each name carries its own write-ahead log", counted))
		}
	}
	if len(reasons) == 0 {
		return StoreComparison{Unproven, "no expectation was supplied to compare against"}
	}
	hasUnproven := false
	for _, r := range reasons {
		hasUnproven = hasUnproven || r.verdict == Unproven
	}
	for _, verdict := range []StoreVerdict{Mismatch, Unproven, Proven} {
		var matched []string
		for _, r := range reasons {
			if r.verdict == verdict {
				matched = append(matched, r.detail)
			}
		}
		if len(matched) > 0 && !(verdict == Proven && hasUnproven) {
			return StoreComparison{verdict, strings.Join(matched, "; ")}
		}
	}
	var agreedDetails []string
	for _, r := range reasons {
		if r.verdict == "" {
			agreedDetails = append(agreedDetails, r.detail)
		}
	}
	return StoreComparison{Unproven, strings.Join(agreedDetails, "; ") + ". Neither a store id nor a device and inode pair is live evidence, so supply a nonce for proof"}
}

func nonceLinks(n *NonceReading) uint64 {
	if n == nil {
		return 0
	}
	return n.Links
}

func comparePhysical(loc Location, expect string, given bool, add func(StoreVerdict, string)) tristate {
	if expect == "" && !given {
		return notCompared
	}
	want := strings.Split(expect, ":")
	switch {
	case len(want) != 2 || loc.Device == 0 || loc.Inode == 0:
		add(Unproven, "physical identity is not comparable here")
		return notCompared
	case want[0] != strconv.FormatUint(loc.Device, 10) || want[1] != strconv.FormatUint(loc.Inode, 10):
		add(Mismatch, fmt.Sprintf("device:inode %d:%d is not %s", loc.Device, loc.Inode, expect))
		return disagreed
	default:
		add("", "device and inode match, which does not say both participants opened the same pathname for that inode")
		return agreed
	}
}

func compareLog(loc Location, expect string, given bool, add func(StoreVerdict, string)) tristate {
	if expect == "" && !given {
		return notCompared
	}
	// <device>:<inode>:<name>, split at most twice so a name containing a colon survives.
	want := strings.SplitN(expect, ":", 3)
	switch {
	case len(want) != 3 || loc.LogDevice == 0 || loc.LogInode == 0 || loc.LogName == "":
		add(Unproven, "the log location is not comparable here")
		return notCompared
	case want[0] != strconv.FormatUint(loc.LogDevice, 10) || want[1] != strconv.FormatUint(loc.LogInode, 10) || want[2] != loc.LogName:
		add(Unproven, fmt.Sprintf("this database's write-ahead log is written beside %d:%d/%s and the other participant reported %s, so the two were not shown to write into one log", loc.LogDevice, loc.LogInode, loc.LogName, expect))
		return disagreed
	default:
		add("", "both participants write their write-ahead log under the same directory entry")
		return agreed
	}
}

func gradeNonce(loc Location, n NonceReading, physical, logLocation tristate, add func(StoreVerdict, string)) {
	if !n.Readable {
		add(Unproven, "the nonce could not be read here: "+n.Detail)
		return
	}
	if !n.Found {
		add(Mismatch, "a nonce written by another participant is not here")
		return
	}
	logUnmeasured := n.LogDevice == 0 || n.LogInode == 0 || n.LogName == "" || loc.LogDevice == 0 || loc.LogInode == 0 || loc.LogName == ""
	switch {
	case n.Device == 0 || n.Inode == 0 || loc.Device == 0 || loc.Inode == 0 || n.Device != loc.Device || n.Inode != loc.Inode:
		add(Unproven, fmt.Sprintf("the nonce was read from device:inode %s, and this comparison is about %s", measured(n.Device, n.Inode), measured(loc.Device, loc.Inode)))
	case logLocation == agreed && logUnmeasured:
		add(Unproven, "the log location the nonce was read through could not be measured, so the nonce cannot be attributed to the log this comparison is about")
	case logLocation == agreed && (n.LogDevice != loc.LogDevice || n.LogInode != loc.LogInode || n.LogName != loc.LogName):
		add(Unproven, "the nonce was read through a pathname whose write-ahead log is not the one this comparison is about")
	case physical == agreed && logLocation == agreed:
		add(Proven, "a nonce written by another participant is readable here, in the file this comparison is about, whose write-ahead log is written where that participant reported writing its own")
	default:
		var wanted []string
		if physical == notCompared {
			wanted = append(wanted, "--expect-inode")
		}
		if logLocation == notCompared {
			wanted = append(wanted, "--expect-log")
		}
		if len(wanted) > 0 {
			add(Unproven, "a nonce written by another participant is readable here, which does not say the two are one live store: a copy taken after the challenge was written carries the nonce with the bytes, and one inode reached at a second pathname keeps a write-ahead log of its own. Supply the other participant's "+strings.Join(wanted, " and "))
		}
	}
}

// measured renders a device:inode pair as Python's f-string renders one holding None.
func measured(device, inode uint64) string {
	part := func(v uint64) string {
		if v == 0 {
			return "None"
		}
		return strconv.FormatUint(v, 10)
	}
	return part(device) + ":" + part(inode)
}

func (l Location) PhysicalIdentity() string {
	return strconv.FormatUint(l.Device, 10) + ":" + strconv.FormatUint(l.Inode, 10)
}

func (l Location) LogLocation() string {
	return fmt.Sprintf("%d:%d:%s", l.LogDevice, l.LogInode, l.LogName)
}
