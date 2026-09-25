package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
	return "rel-" + digest(parent+"|"+child+"|"+issue, 16), nil
}

func EventID(relationship string, generation int, revision, outcome, turn string, attempt *int) (string, error) {
	if generation < 1 {
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
		return digest(fmt.Sprintf("%s|%d|%s|%s|%s", relationship, generation, outcome, turn, RenderAttempt(attempt)), 32), nil
	default:
		return "", ErrInvalidIdentity
	}
}

// Outcomes are every outcome a completion receipt may carry (identity.OUTCOMES).
var Outcomes = []string{"ready_for_review", "failed", "interrupted", "blocked_needs_input"}

// RenderAttempt is identity.render_attempt: an explicit nil test, so zero renders as "0" and
// is never quietly treated as the literal "null".
func RenderAttempt(attempt *int) string {
	if attempt == nil {
		return "null"
	}
	return strconv.Itoa(*attempt)
}

func RequestID(event string, number int) (string, error) {
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(event) || number < 1 {
		return "", ErrInvalidIdentity
	}
	return fmt.Sprintf("del-%s-a%d", event[:12], number), nil
}

func ParseRequestID(request string) (string, int, error) {
	match := regexp.MustCompile(`^del-([0-9a-f]{12})-a([0-9]+)$`).FindStringSubmatch(request)
	if match == nil {
		return "", 0, ErrInvalidIdentity
	}
	number, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, fmt.Errorf("parse attempt: %w", err)
	}
	return match[1], number, nil
}

func AckProof(event, turn string) (string, error) {
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(event) || strings.TrimSpace(turn) == "" {
		return "", ErrInvalidIdentity
	}
	return digest(event+"|"+turn, 64), nil
}

func RevisionRequestEventID(relationship, event, turn string) (string, error) {
	if strings.TrimSpace(event) == "" || strings.TrimSpace(turn) == "" {
		return "", ErrInvalidIdentity
	}
	return digest(relationship+"|"+event+"|needs_changes_revision|"+turn+"|null", 32), nil
}
