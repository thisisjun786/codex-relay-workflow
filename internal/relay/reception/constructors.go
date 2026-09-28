package reception

import "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"

func PullRequest(repository, number, head, base, url any) (Obj, error) {
	for _, p := range []struct {
		k string
		v any
	}{{"repository", repository}, {"number", number}, {"head_sha", head}} {
		if !present(p.v) {
			return nil, malformed("a pull request artifact states its %s", p.k)
		}
	}
	return O("kind", "pull_request", "repository", evidence.Text(repository), "number", number, "headSha", evidence.Text(head), "baseSha", base, "url", url), nil
}
func Locator(path, hash, produced any) (Obj, error) {
	for _, p := range []struct {
		k string
		v any
	}{{"path", path}, {"digest", hash}} {
		if !present(p.v) {
			return nil, malformed("an artifact locator states its %s; a deliverable nobody can hash is not one this can identify", p.k)
		}
	}
	return O("kind", "locator", "path", evidence.Text(path), "digest", evidence.Text(hash), "producedAt", produced), nil
}
func Policy(model, effort, workflow, mode, sandbox, approval any) (Obj, error) {
	for _, p := range []struct {
		k string
		v any
	}{{"model", model}, {"effort", effort}, {"workflow", workflow}, {"mode", mode}} {
		if !present(p.v) {
			return nil, malformed("a policy states its %s; an unstated one is read as whatever the recipient already had", p.k)
		}
	}
	return O("model", evidence.Text(model), "effort", evidence.Text(effort), "workflow", evidence.Text(workflow), "mode", evidence.Text(mode), "sandbox", sandbox, "approval", approval), nil
}
func Callback(task, model, effort any) (Obj, error) {
	for _, p := range []struct {
		k string
		v any
	}{{"task_id", task}, {"model", model}, {"effort", effort}} {
		if !present(p.v) || evidence.TypeName(p.v) != "str" {
			return nil, malformed("a callback states its %s as text; one without it names nowhere to answer or no pair to answer under", p.k)
		}
	}
	return O("taskId", task, "model", model, "effort", effort), nil
}
