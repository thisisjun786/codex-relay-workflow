package gui

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// The screen tree this binary carries is the built one, not a placeholder: an index.html and at
// least one hashed JavaScript asset, and the index names the script it needs so the two cannot
// drift apart. CRW-829 shipped a placeholder index.html; this issue replaces it with the build of
// web/, and these tests are what makes the replacement real rather than an empty directory.
func TestEmbeddedAssetsHoldTheBuiltScreen(t *testing.T) {
	index, err := fs.ReadFile(assetFS, "assets/index.html")
	if err != nil {
		t.Fatalf("the built index.html is not embedded: %v", err)
	}
	if strings.Contains(string(index), "screens are not built into this binary yet") {
		t.Fatal("the CRW-829 placeholder is still the embedded index.html")
	}
	if !strings.Contains(string(index), `id="root"`) {
		t.Fatalf("the embedded index.html has no #root mount point: %q", index)
	}
	scripts, err := fs.Glob(assetFS, "assets/assets/*.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) == 0 {
		t.Fatal("no JavaScript asset is embedded under assets/assets/")
	}
	// The built index references its script by the hashed name, so an embedded index whose script
	// is missing would serve a screen that never boots.
	name := strings.TrimPrefix(scripts[0], "assets/")
	if !strings.Contains(string(index), name) {
		t.Errorf("the embedded index.html does not name the embedded script %q", name)
	}
}

// GET / serves the embedded index.html byte for byte, which is what an operator sees when the
// server prints its URL. The tree is the real embedded one, not a fixture, so this fails if the
// assets are ever absent from the binary.
func TestServerServesTheEmbeddedIndexAtRoot(t *testing.T) {
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "test-version"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want, err := fs.ReadFile(assetFS, indexFile)
	if err != nil {
		t.Fatalf("the embedded index.html is missing: %v", err)
	}
	got := request(server, http.MethodGet, "/", guardHost, "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("GET /: status %d", got.Code)
	}
	if got.Body.String() != string(want) {
		t.Errorf("GET / did not serve the embedded index.html (%d bytes served, %d embedded)",
			got.Body.Len(), len(want))
	}
	if ct := got.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content type %q", ct)
	}
}
