package managed

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// requestWithTitle is the request fixture with child.title replaced by title.
func requestWithTitle(t *testing.T, title string) []byte {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal(requestFixture(t), &r); err != nil {
		t.Fatal(err)
	}
	r["child"].(map[string]any)["title"] = title
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The managed request bounds child.title in characters while the bridge bounds a create's title in
// bytes, so a title of 500 characters or fewer that is longer than 500 bytes is accepted and the
// request is armed, and only then does the bridge refuse the create. The request checks the byte
// bound too, before anything is armed, with the same kind of validation error the character bound
// gives.
func TestParseRequestRefusesATitleOver500Bytes(t *testing.T) {
	t.Parallel()
	// 200 Korean characters are 600 bytes: inside the character bound and over the byte bound.
	long := koreanTitle(600)
	if utf8.RuneCountInString(long) != 200 || len(long) != 600 {
		t.Fatalf("the fixture is %d runes and %d bytes", utf8.RuneCountInString(long), len(long))
	}
	if err := text(long, "child.title", 500); err != nil {
		t.Fatalf("the character bound already refuses the fixture, so the red is not the byte bound: %v", err)
	}
	_, err := ParseRequest(requestWithTitle(t, long))
	if err == nil {
		t.Fatalf("a %d-byte title was accepted", len(long))
	}
	if !strings.Contains(err.Error(), "500 bytes") {
		t.Fatalf("the refusal does not name bytes: %v", err)
	}
}

// The boundary: exactly 500 bytes is accepted, 501 bytes is refused.
func TestParseRequestTitleByteBoundary(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		title string
		ok    bool
	}{
		{"exactly 500 bytes", asciiTitle(500), true},
		{"501 bytes", asciiTitle(501), false},
		{"exactly 500 bytes of Korean", koreanTitle(498) + "ab", true},
		{"501 bytes of Korean", koreanTitle(498) + "abc", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if len(c.title) != 500 && len(c.title) != 501 {
				t.Fatalf("the fixture is %d bytes", len(c.title))
			}
			_, err := ParseRequest(requestWithTitle(t, c.title))
			if c.ok && err != nil {
				t.Fatalf("a %d-byte title was refused: %v", len(c.title), err)
			}
			if !c.ok && err == nil {
				t.Fatalf("a %d-byte title was accepted", len(c.title))
			}
		})
	}
}

// The other fields keep the character bound: a 500-character host id of two-byte runes is accepted,
// so the byte check was added for child.title alone and no other field's message changed.
func TestParseRequestOtherFieldsKeepTheirCharacterBound(t *testing.T) {
	t.Parallel()
	var r map[string]any
	if err := json.Unmarshal(requestFixture(t), &r); err != nil {
		t.Fatal(err)
	}
	// 500 two-byte runes is 1000 bytes: inside the character bound, over the byte bound. The two
	// host ids must still match, so both move together.
	host := strings.Repeat("é", 500)
	r["parent"].(map[string]any)["hostId"] = host
	r["child"].(map[string]any)["hostId"] = host
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRequest(raw); err != nil {
		t.Fatalf("a 500-character host id was refused: %v", err)
	}
	_, err = ParseRequest(requestWithTitle(t, strings.Repeat("é", 500)))
	if err == nil || !strings.Contains(err.Error(), "child.title") {
		t.Fatalf("the title's byte refusal: %v", err)
	}
}
