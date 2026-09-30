package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var ErrInvalidIdentity = errors.New("store: invalid identity input")

func digest(text string, width int) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:width]
}

func RelationshipID(parent, child, issue string) (string, error) {
	for _, value := range []string{parent, child, issue} {
		if strings.TrimSpace(value) == "" || strings.Contains(value, "|") {
			return "", ErrInvalidIdentity
		}
	}
	// str.encode("utf-8") of the joined fields raises for one holding a surrogate escape (an argv
	// byte that is not UTF-8), before any row is read or written.
	if err := EncodeUTF8(parent + "|" + child + "|" + issue); err != nil {
		return "", err
	}
	return "rel-" + digest(parent+"|"+child+"|"+issue, 16), nil
}

func EventID(relationship string, generation int, revision, outcome, turn string, attempt *int) (string, error) {
	var n *big.Int
	if attempt != nil {
		n = big.NewInt(int64(*attempt))
	}
	return EventIDBig(relationship, big.NewInt(int64(generation)), revision, outcome, turn, n)
}

// EventIDBig retains Python integer identity before any SQLite narrowing.
func EventIDBig(relationship string, generation *big.Int, revision, outcome, turn string, attempt *big.Int) (string, error) {
	if generation.Sign() < 1 {
		return "", ErrInvalidIdentity
	}
	switch outcome {
	case "ready_for_review":
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(revision) || revision == strings.Repeat("0", 64) {
			return "", ErrInvalidIdentity
		}
		return digest(fmt.Sprintf("%s|%d|%s|%s", relationship, generation, revision, outcome), 32), nil
	case "failed", "interrupted", "blocked_needs_input":
		if strings.TrimSpace(turn) == "" {
			return "", ErrInvalidIdentity
		}
		return digest(fmt.Sprintf("%s|%d|%s|%s|%s", relationship, generation, outcome, turn, renderBigAttempt(attempt)), 32), nil
	default:
		return "", ErrInvalidIdentity
	}
}

func renderBigAttempt(n *big.Int) string {
	if n == nil {
		return "null"
	}
	return n.String()
}

// IntegerOverflow is sqlite3's Python integer binding failure.
type IntegerOverflow struct{}

func (*IntegerOverflow) Error() string {
	return "OverflowError: Python int too large to convert to SQLite INTEGER"
}

func RequestID(event string, number int) (string, error) {
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(event) || number < 1 {
		return "", ErrInvalidIdentity
	}
	return fmt.Sprintf("del-%s-a%d", event[:12], number), nil
}

func RevisionRequestEventID(relationship, event, turn string) (string, error) {
	if strings.TrimSpace(event) == "" || strings.TrimSpace(turn) == "" {
		return "", ErrInvalidIdentity
	}
	return digest(relationship+"|"+event+"|needs_changes_revision|"+turn+"|null", 32), nil
}
