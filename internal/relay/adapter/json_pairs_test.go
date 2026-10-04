package adapter

import "testing"

// CRW-454: the receipt of a creation under a role that may run on several pairs lists those pairs
// in a fixed order, as the rest of the receipt is ordered, and a role with one pair prints as before.
func TestAReceiptOrdersThePairsOfAMultiPairRole(t *testing.T) {
	pair := func(model string) map[string]any { return map[string]any{"reasoningEffort": "xhigh", "model": model} }
	several := map[string]any{"overriddenBy": nil, "pairs": []any{pair("a"), pair("b")}, "reasoningEffort": "xhigh", "model": "b", "expectation": "pair", "role": "child"}
	const wantSeveral = "{\"role\": \"child\", \"expectation\": \"pair\", \"model\": \"b\", \"reasoningEffort\": \"xhigh\", \"pairs\": [{\"model\": \"a\", \"reasoningEffort\": \"xhigh\"}, {\"model\": \"b\", \"reasoningEffort\": \"xhigh\"}], \"overriddenBy\": null}"
	if got := dumps(bridgeValue("roleExpectation", several), false); got != wantSeveral {
		t.Fatalf("got %s", got)
	}
	single := map[string]any{"overriddenBy": nil, "reasoningEffort": "xhigh", "model": "a", "expectation": "pair", "role": "parent"}
	const wantSingle = "{\"role\": \"parent\", \"expectation\": \"pair\", \"model\": \"a\", \"reasoningEffort\": \"xhigh\", \"overriddenBy\": null}"
	if got := dumps(bridgeValue("roleExpectation", single), false); got != wantSingle {
		t.Fatalf("got %s", got)
	}
}
