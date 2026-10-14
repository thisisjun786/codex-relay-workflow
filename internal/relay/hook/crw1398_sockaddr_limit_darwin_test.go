package hook

import "testing"

func TestSockaddrLimitOnDarwinIs104(t *testing.T) {
	if sockaddrUnPathLimit != 104 {
		t.Fatalf("sockaddrUnPathLimit = %d on darwin, want 104", sockaddrUnPathLimit)
	}
}
