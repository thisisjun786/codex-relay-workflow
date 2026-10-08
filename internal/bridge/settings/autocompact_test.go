package settings

import (
	"slices"
	"testing"
)

// The config object is what thread/start and thread/resume carry, so the limit travels on both. A
// contract that holds no limit sends no key at all, which is today's behaviour.
func TestConfigAndResumeParamsCarryTheAutoCompactLimitOnlyWhenItIsSet(t *testing.T) {
	limit := int64(550000)
	set := Contract{CWD: "/w", Model: "m", ReasoningEffort: "high", AutoCompactTokenLimit: &limit}
	if got := set.Config()[AutoCompactTokenLimitKey]; got != int64(550000) {
		t.Fatalf("Config() = %v", set.Config())
	}
	for _, params := range []map[string]any{set.StartParams(), set.ResumeParams("thread-1")} {
		if got := params["config"].(map[string]any)[AutoCompactTokenLimitKey]; got != int64(550000) {
			t.Fatalf("params = %v", params)
		}
	}
	without := Contract{CWD: "/w", Model: "m", ReasoningEffort: "high"}
	if _, present := without.Config()[AutoCompactTokenLimitKey]; present {
		t.Fatalf("Config() = %v", without.Config())
	}
	if _, present := without.ResumeParams("thread-1")["config"].(map[string]any)[AutoCompactTokenLimitKey]; present {
		t.Fatalf("ResumeParams = %v", without.ResumeParams("thread-1"))
	}
}

// The host has no field to report this in, so its silence is not a mismatch and never withholds a
// message; the receipt records the value as requested and unobservable, and never as verified.
func TestTheReceiptRecordsTheLimitAsUnobservableAndNeverWithholds(t *testing.T) {
	limit := int64(550000)
	c := Contract{Model: "m", ReasoningEffort: "high", AutoCompactTokenLimit: &limit}
	response := map[string]any{"approvalPolicy": "never", "model": "m", "reasoningEffort": "high"}
	if findings := c.Findings(response); len(findings) != 0 {
		t.Fatalf("the host's silence about the limit was read as a mismatch: %v", findings)
	}
	receipt := c.Receipt(response, "resume")
	if receipt["verification"] != "observed_at_resume" {
		t.Fatalf("verification = %v", receipt["verification"])
	}
	if !slices.Contains(receipt["unobservable"].([]string), AutoCompactTokenLimitKey) {
		t.Fatalf("unobservable = %v", receipt["unobservable"])
	}
	if slices.Contains(receipt["verified"].([]string), AutoCompactTokenLimitKey) {
		t.Fatalf("verified = %v", receipt["verified"])
	}
	if got := receipt["requested"].(map[string]any)[AutoCompactTokenLimitKey]; got != int64(550000) {
		t.Fatalf("requested = %v", receipt["requested"])
	}
}

// A contract whose only setting is the limit has observed nothing, so its receipt says so rather
// than claiming a verification the host never gave.
func TestALimitAloneIsNotAVerification(t *testing.T) {
	limit := int64(1)
	receipt := (&Contract{AutoCompactTokenLimit: &limit}).Receipt(map[string]any{"approvalPolicy": "never"}, "resume")
	if receipt["verification"] != "not_requested" || len(receipt["verified"].([]string)) != 0 {
		t.Fatalf("receipt = %v", receipt)
	}
}
