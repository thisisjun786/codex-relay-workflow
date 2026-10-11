package fakehost

// SchemaServerParams is the params object the generated codex schema (ServerRequest.json) defines for
// one approval-class server request, about the given thread. The two legacy approvals name the thread
// in conversationId and have no threadId; every other method names it in threadId.
func SchemaServerParams(method, threadID, turnID string) map[string]any {
	switch method {
	case "execCommandApproval":
		return map[string]any{
			"callId": "call-1", "conversationId": threadID, "command": []string{"rm", "-rf", "/"},
			"cwd": "/work", "parsedCmd": []any{map[string]any{"type": "unknown", "cmd": "rm -rf /"}},
		}
	case "applyPatchApproval":
		return map[string]any{"callId": "call-1", "conversationId": threadID, "fileChanges": map[string]any{}}
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return map[string]any{"itemId": "item-1", "startedAtMs": 1, "threadId": threadID, "turnId": turnID}
	case "item/permissions/requestApproval":
		return map[string]any{"cwd": "/work", "itemId": "item-1", "permissions": map[string]any{}, "startedAtMs": 1, "threadId": threadID, "turnId": turnID}
	case "mcpServer/elicitation/request":
		return map[string]any{"serverName": "server-1", "threadId": threadID, "turnId": turnID, "mode": "url", "message": "approve?", "elicitationId": "elicit-1", "url": "https://example.invalid/approve"}
	case "item/tool/requestUserInput":
		return map[string]any{
			"isBlocking": true, "itemId": "item-1", "threadId": threadID, "turnId": turnID,
			"questions": []any{map[string]any{"header": "Choose", "id": "q1", "question": "Which?"}},
		}
	}
	return map[string]any{"threadId": threadID, "turnId": turnID}
}
