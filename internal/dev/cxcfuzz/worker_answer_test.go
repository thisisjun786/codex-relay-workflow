//go:build dev

package cxcfuzz

import "testing"

// An error reply keeps a lone surrogate in its message as the same escape a success reply keeps, so the
// comparison sees the oracle's text and not U+FFFD (CRW-978 c5, CRW-971 item 2).
func TestAnswerKeepsALoneSurrogateInAnErrorMessage(t *testing.T) {
	got, err := answer(1, `{"id":1,"error":{"name":"Error","message":"\ud800"}}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	value, err := decode(got)
	if err != nil {
		t.Fatalf("the error answer %q does not decode: %v", got, err)
	}
	errorObject, _ := field(value, "error")
	message, _ := field(errorObject, "message")
	if message != "\xed\xa0\x80" {
		t.Fatalf("the error message holds %q, want the lone surrogate kept (%q)", message, "\xed\xa0\x80")
	}
}

// Control: an error message with no surrogate comes back unchanged.
func TestAnswerLeavesAnErrorMessageWithoutASurrogateUnchanged(t *testing.T) {
	got, err := answer(2, `{"id":2,"error":{"name":"TypeError","message":"plain text"}}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"error":{"name":"TypeError","message":"plain text"}}`; got != want {
		t.Fatalf("the error answer %s, want %s", got, want)
	}
}
