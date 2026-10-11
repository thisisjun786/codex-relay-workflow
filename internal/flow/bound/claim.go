package bound

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// A claim is the hook's mark that an engine call has spent its native helper: claims/<thread_id>/<call_id>, a file that holds
// {tool_use_id, input} of the spawn that made it. It is made whole by a hard link, so of two spawns that race one wins, and
// nobody ever reads a half-written claim. The answer the hook gave the winner is kept beside it as <call_id>.answer, written
// whole by a rename, so the same event delivered again gets the same answer. Both live in the flow-bound directory, outside the
// workspace an executor can write.

// Claim is the content of a claim file.
type Claim struct {
	// ToolUseID is the tool use id of the spawn that made the claim.
	ToolUseID string `json:"tool_use_id"`
	// Input is the spawn's original tool_input as the hook writes it (JSON, compact).
	Input string `json:"input"`
}

// maxClaim bounds what is read back: the hook's 4 MiB input with the framing of the claim, and an answer with room for the pair.
const maxClaim = 16 << 20

// ClaimFile is the name of the claim of a call under the thread's claims directory; AnswerFile the name of its answer.
func ClaimFile(call string) string  { return call }
func AnswerFile(call string) string { return call + ".answer" }

// claimRoot opens claims/<thread> under dir as a root, creating the two directories (mode 0700) when they are not there. Every
// directory on the way must be the current user's, mode 0700 and no symlink.
func claimRoot(dir, thread string) (*os.Root, error) {
	if !ValidThreadID(thread) {
		return nil, unavailable(ReasonClaim, "%q is not a thread id", thread)
	}
	top, err := openDir(dir)
	if err != nil {
		return nil, err
	}
	defer top.Close()
	return subRoot(top, ClaimsDir, thread)
}

// subRoot opens each name in turn below root, creating them when create is set.
func subRoot(root *os.Root, names ...string) (*os.Root, error) {
	cur, owned := root, false
	for _, name := range names {
		next, err := openSub(cur, name, true)
		if owned {
			cur.Close()
		}
		if err != nil {
			return nil, err
		}
		cur, owned = next, true
	}
	return cur, nil
}

// openSub opens the directory name below parent as a root, making it first when it is not there. The directory must be a
// real directory of the current user with mode 0700.
func openSub(parent *os.Root, name string, create bool) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil && errorsIsAbsent(err) && !create {
		return nil, errAbsentSub
	}
	if err != nil && errorsIsAbsent(err) {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, unavailable(ReasonClaim, "%s: %v", name, err)
		} else if err == nil {
			_ = parent.Chmod(name, 0o700) // a restrictive umask may have taken bits off
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, unavailable(ReasonClaim, "%s: %v", name, err)
	}
	if bad := trusted(info, name, true, 0o700); bad != nil {
		return nil, bad
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, unavailable(ReasonClaim, "%s: %v", name, err)
	}
	after, err := sub.Stat(".")
	if err != nil || !os.SameFile(info, after) {
		sub.Close()
		return nil, unavailable(ReasonClaim, "%s changed while it was opened", name)
	}
	return sub, nil
}

// CreateClaim makes the claim of call for c. created is false, with a nil error, when the claim is already there (it was made
// by this spawn delivered again, or by another spawn). The claim is written to a file of its own, synced, and linked into
// place, so it is whole the moment it exists.
func CreateClaim(dir, thread, call string, c Claim) (created bool, err error) {
	if !ValidCallID(call) {
		return false, unavailable(ReasonClaim, "the call id %q cannot name a claim", call)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	root, err := claimRoot(dir, thread)
	if err != nil {
		return false, err
	}
	defer root.Close()
	tmp, err := writeTemp(root, "claim-"+call, raw, true)
	if err != nil {
		return false, unavailable(ReasonClaim, "%v", err)
	}
	defer root.Remove(tmp)
	if err := root.Link(tmp, ClaimFile(call)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, unavailable(ReasonClaim, "%v", err)
	}
	if d, err := root.Open("."); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return true, nil
}

// ReadClaim reads the claim of call; present is false when there is none.
func ReadClaim(dir, thread, call string) (c Claim, present bool, err error) {
	raw, present, err := readClaimFile(dir, thread, ClaimFile(call), call)
	if err != nil || !present {
		return Claim{}, present, err
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.ToolUseID == "" {
		return Claim{}, true, unavailable(ReasonClaim, "the claim of call %s cannot be read", call)
	}
	return c, true, nil
}

// ReadAnswer reads the answer stored for call; present is false when there is none.
func ReadAnswer(dir, thread, call string) (answer string, present bool, err error) {
	raw, present, err := readClaimFile(dir, thread, AnswerFile(call), call)
	return string(raw), present, err
}

func readClaimFile(dir, thread, name, call string) ([]byte, bool, error) {
	if !ValidCallID(call) {
		return nil, false, unavailable(ReasonClaim, "the call id %q cannot name a claim", call)
	}
	root, err := openClaimsIfThere(dir, thread)
	if err != nil || root == nil {
		return nil, false, err
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errorsIsAbsent(err) {
			return nil, false, nil
		}
		return nil, true, unavailable(ReasonClaim, "%s: %v", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, true, unavailable(ReasonClaim, "%s: %v", name, err)
	}
	if bad := trusted(info, name, false, 0o600); bad != nil {
		return nil, true, bad
	}
	if info.Size() > maxClaim {
		return nil, true, unavailable(ReasonClaim, "%s is larger than %d bytes", name, maxClaim)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxClaim+1))
	if err != nil {
		return nil, true, unavailable(ReasonClaim, "%s: %v", name, err)
	}
	return raw, true, nil
}

// openClaimsIfThere opens claims/<thread> without making anything; nil when it is not there.
func openClaimsIfThere(dir, thread string) (*os.Root, error) {
	if !ValidThreadID(thread) {
		return nil, unavailable(ReasonClaim, "%q is not a thread id", thread)
	}
	top, err := openDir(dir)
	if err != nil {
		return nil, err
	}
	defer top.Close()
	claims, err := openSub(top, ClaimsDir, false)
	if err != nil {
		if isAbsentSub(err) {
			return nil, nil
		}
		return nil, err
	}
	defer claims.Close()
	sub, err := openSub(claims, thread, false)
	if err != nil {
		if isAbsentSub(err) {
			return nil, nil
		}
		return nil, err
	}
	return sub, nil
}

// errAbsentSub is what openSub answers for a directory that is not there when it was asked not to make it.
var errAbsentSub = errors.New("bound: the directory is not there")

// isAbsentSub is whether openSub failed because the directory is not there.
func isAbsentSub(err error) bool { return errors.Is(err, errAbsentSub) }

// WriteAnswer stores the answer given for call, whole, by a rename. The same answer written twice (two deliveries of one
// event) is the same file.
func WriteAnswer(dir, thread, call, answer string) error {
	if !ValidCallID(call) {
		return unavailable(ReasonClaim, "the call id %q cannot name a claim", call)
	}
	root, err := claimRoot(dir, thread)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp, err := writeTemp(root, "answer-"+call, []byte(answer), true)
	if err != nil {
		return unavailable(ReasonClaim, "%v", err)
	}
	if err := root.Rename(tmp, AnswerFile(call)); err != nil {
		_ = root.Remove(tmp)
		return unavailable(ReasonClaim, "%v", err)
	}
	if d, err := root.Open("."); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// writeTemp writes data to a new file with mode 0600 in root under a name that no other writer uses, optionally synced, and
// returns the name.
func writeTemp(root *os.Root, base string, data []byte, sync bool) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	name := "." + base + ".tmp-" + hex.EncodeToString(suffix[:])
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil && sync {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = root.Remove(name)
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return name, nil
}
