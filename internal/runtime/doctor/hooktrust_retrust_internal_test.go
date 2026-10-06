package doctor

import (
	"os"
	"strings"
	"testing"
)

// TestHookTrustRetrustVerifyNext covers the package-internal step the command line cannot isolate:
// next is proven in a temporary Codex home, and a failure refuses with the issue's message and
// publishes nothing. The real config.toml is untouched because this function never writes to it;
// crwdir's own tests prove the exchange keeps what it displaced.
func TestHookTrustRetrustVerifyNext(t *testing.T) {
	f := newCASFixture(t, "")
	original := "model = \"gpt-5.5\"\n"
	f.write(f.config(), original)
	// A next that trusts both declared hooks is what a passing probe accepts; the same bytes with a
	// failing probe are refused, and the real config.toml is never touched either way.
	trusted := original
	for _, entry := range f.entries {
		trusted += "[hooks.state.\"" + entry.Key + "\"]\ntrusted_hash = \"" + entry.Hash + "\"\n"
	}

	err := hookTrustRetrustVerifyNext(f.plugin, f.key, trusted, failingRunner)
	if err == nil || !strings.Contains(err.Error(), "pre-write verification failed") || !strings.Contains(err.Error(), "config.toml unchanged") {
		t.Fatalf("the refusal text is wrong: %v", err)
	}
	if got := f.read(f.config()); got != original {
		t.Fatalf("the refusal changed config.toml: %q", got)
	}
	// The temporary home is removed: the real home still holds only config.toml.
	entries, rerr := os.ReadDir(f.home)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, entry := range entries {
		if entry.Name() != "config.toml" {
			t.Fatalf("the verification left %q in the real home", entry.Name())
		}
	}
	// A passing probe accepts the same next.
	if err := hookTrustRetrustVerifyNext(f.plugin, f.key, trusted, okRunner); err != nil {
		t.Fatalf("a good next was refused: %v", err)
	}
	// A next that does not trust the hooks is refused even with a passing probe: the pre-write check
	// is the post-write diagnosis moved ahead of the publication.
	err = hookTrustRetrustVerifyNext(f.plugin, f.key, original, okRunner)
	if err == nil || !strings.Contains(err.Error(), "post-write verification failed") {
		t.Fatalf("an untrusted next was accepted: %v", err)
	}
}
