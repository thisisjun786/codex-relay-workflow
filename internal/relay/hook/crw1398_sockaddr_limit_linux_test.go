package hook

import "testing"

func TestSockaddrLimitOnLinuxIs108(t *testing.T) {
	if sockaddrUnPathLimit != 108 {
		t.Fatalf("sockaddrUnPathLimit = %d on Linux, want 108", sockaddrUnPathLimit)
	}
}
