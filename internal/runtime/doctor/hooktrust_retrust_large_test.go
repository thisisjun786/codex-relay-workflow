package doctor_test

import (
	"strings"
	"testing"
)

// CRW-1152 round 2: the harness's 4 MiB diagnostic read limit is the harness's own. A valid
// config.toml past it (a long comment) is still retrusted: the pre-write verification reads the
// candidate config whole, as it did before the harness bound existed.
func TestHookTrustRetrust_initializes_a_config_past_the_harness_read_limit(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), "# "+strings.Repeat("x", 5<<20)+"\n"+f.installed())

	stdout, stderr, code := f.run("--bootstrap-ok")
	if code != 0 || stderr != "" {
		t.Fatalf("bootstrap: code=%d stderr=%q stdout=%.400q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "updated=0 appended=2"+"\n") {
		t.Fatalf("the answer does not report two appended sections: %.400q", stdout)
	}
	content := f.read(f.config())
	for n := range f.entries {
		if !strings.Contains(content, f.section(n, f.entries[n].Hash)) {
			t.Fatalf("config.toml does not carry entry %d", n)
		}
	}
}
