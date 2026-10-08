package migrate

// apply_owned_dir_identity_unsupported_linux_test.go holds the CRW-987 d2 cases: a Linux kernel that has neither fchmodat2
// nor a usable /proc descriptor path is refused as unsupported, naming both mechanisms, while a genuine chmod failure on a
// mechanism that exists stays an ordinary failure with its errno.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// migrateFollowupChmodSeams replaces the two descriptor-chmod mechanisms for one case and restores them afterwards.
func migrateFollowupChmodSeams(t *testing.T, fchmodat2 func(int, uint32) error, proc func(int, uint32) error) {
	t.Helper()
	restore2, restoreProc := ownedDirIdentityFchmodat2, ownedDirIdentityProcChmod
	t.Cleanup(func() { ownedDirIdentityFchmodat2, ownedDirIdentityProcChmod = restore2, restoreProc })
	ownedDirIdentityFchmodat2, ownedDirIdentityProcChmod = fchmodat2, proc
}

// CRW-987 d2: when neither mechanism can be used, the run is refused as unsupported and the report names both mechanisms.
// The head before this cycle returned the /proc errno, which the report read as a generic failure with no reason.
func TestMigrateFollowupNoChmodMechanismIsRefusedAsUnsupported(t *testing.T) {
	cases := []struct {
		name      string
		fchmodat2 error
		procChmod error
	}{
		{"fchmodat2 ENOSYS, /proc ENOENT", unix.ENOSYS, unix.ENOENT},
		{"fchmodat2 EOPNOTSUPP, /proc EACCES", unix.EOPNOTSUPP, unix.EACCES},
		{"fchmodat2 ENOSYS, /proc ENOTDIR", unix.ENOSYS, unix.ENOTDIR},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			migrateFollowupChmodSeams(t,
				func(int, uint32) error { return tc.fchmodat2 },
				func(int, uint32) error { return tc.procChmod })
			_, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
			_, err := apply(r, p)
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Reason != ReasonUnsupported {
				t.Fatalf("want a refusal with reason unsupported, got %v", err)
			}
			if !strings.Contains(refused.Detail, "fchmodat2") || !strings.Contains(refused.Detail, "/proc/self/fd") {
				t.Errorf("the refusal must name both mechanisms, detail = %q", refused.Detail)
			}
			rep := reportError(err)
			if rep.Kind != string(ResultRefused) || rep.Reason != string(ReasonUnsupported) {
				t.Errorf("the report must read refused with reason unsupported, got kind %q reason %q", rep.Kind, rep.Reason)
			}
		})
	}
}

// CRW-987 d2 control: a kernel without fchmodat2 whose /proc descriptor path works takes that path and finishes the mode.
func TestMigrateFollowupMissingFchmodat2UsesAUsableProcPath(t *testing.T) {
	procs := 0
	migrateFollowupChmodSeams(t,
		func(int, uint32) error { return unix.ENOSYS },
		func(fd int, perm uint32) error {
			procs++
			return unix.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), perm)
		})
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	if _, err := apply(r, p); err != nil {
		t.Fatalf("a usable /proc path must finish the directory: %v", err)
	}
	if procs == 0 {
		t.Error("the run never took the /proc descriptor path")
	}
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
}

// CRW-987 d2 control: a genuine chmod failure on a mechanism that exists is an ordinary failure with its own errno, never a
// refusal as unsupported.
func TestMigrateFollowupGenuineChmodFailureKeepsItsErrno(t *testing.T) {
	for _, errno := range []error{unix.EPERM, unix.EIO, unix.EROFS} {
		migrateFollowupChmodSeams(t,
			func(int, uint32) error { return unix.ENOSYS },
			func(int, uint32) error { return errno })
		_, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
		_, err := apply(r, p)
		var refused *RefusedError
		if errors.As(err, &refused) {
			t.Errorf("%v on a mechanism that exists must not be a refusal, got %v", errno, err)
		}
		if !errors.Is(err, errno) {
			t.Errorf("the failure must keep its errno %v, got %v", errno, err)
		}
	}
}

// CRW-987 d3 (review): a genuine refusal from fchmodat2 is the mechanism's own failure. The /proc path must not get past it,
// even where that path would succeed, so the error keeps its errno.
func TestMigrateFollowupGenuineFchmodat2FailureIsNotHiddenByProc(t *testing.T) {
	for _, errno := range []error{unix.EPERM, unix.EIO, unix.EROFS} {
		migrateFollowupChmodSeams(t,
			func(int, uint32) error { return errno },
			func(fd int, perm uint32) error {
				return unix.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), perm)
			})
		_, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
		_, err := apply(r, p)
		var refused *RefusedError
		if errors.As(err, &refused) {
			t.Errorf("%v from fchmodat2 must not be a refusal, got %v", errno, err)
		}
		if !errors.Is(err, errno) {
			t.Errorf("the failure must keep fchmodat2's errno %v, got %v", errno, err)
		}
	}
}
