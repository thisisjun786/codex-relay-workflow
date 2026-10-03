package job

// These are bg-wake's raw envelopes, not PABCD's normalized/truncated context.
// The store serializer keeps JSON.stringify's key order and string escaping.
func contextEnvelope(event, text string) string {
	b, _ := object(Event{{"hookSpecificOutput", Event{{"hookEventName", event}, {"additionalContext", text}}}}, 0)
	return string(b)
}

func blockEnvelope(reason string) string {
	b, _ := object(Event{{"decision", "block"}, {"reason", reason}}, 0)
	return string(b)
}
