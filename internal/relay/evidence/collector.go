package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

// Runner is the read-only forge process seam. Tests feed the same scripted gh transcript to
// Python and Go; production supplies an argv runner and never a network-specific mock.
type Runner func(argv []string, timeout time.Duration) (code int, stdout, stderr string, err error)

type Forge struct {
	Run                              Runner
	Command                          []string
	PageSize, PageBudget, CallBudget int64
	Timeout                          time.Duration
	TimeoutSeconds                   string // exact CLI integer for timeout diagnostics
	Calls                            []map[string]any
	Now                              func() string
}

type Unreadable struct {
	Where, Detail string
	Status        int
}

func (e *Unreadable) Error() string { return e.Detail }

func NewForge(run Runner) *Forge {
	return &Forge{Run: run, Command: []string{"gh"}, PageSize: 100, PageBudget: 50, CallBudget: 300, Timeout: 60 * time.Second, Now: func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00") }}
}
func (f *Forge) execute(argv []string, where string) (any, error) {
	if countReached(len(f.Calls), f.CallBudget) {
		return nil, &Unreadable{Where: where, Detail: fmt.Sprintf("the collection reached its budget of %s forge calls before it finished, so what it has is a prefix rather than an answer", pyvalue.Str(f.CallBudget))}
	}
	full := append(slices.Clone(f.Command), argv...)
	code, out, stderr, err := f.Run(full, f.Timeout)
	call := map[string]any{"argv": full, "exitCode": code}
	if err != nil {
		call["exitCode"] = nil
		f.Calls = append(f.Calls, call)
		if errors.Is(err, context.DeadlineExceeded) {
			seconds := f.TimeoutSeconds
			if seconds == "" {
				seconds = strconv.FormatInt(int64(f.Timeout/time.Second), 10)
			}
			return nil, &Unreadable{Where: where, Detail: fmt.Sprintf("reading %s exceeded the timeout of %s seconds, so no answer was observed", where, seconds)}
		}
		return nil, err
	}
	f.Calls = append(f.Calls, call)
	if code != 0 {
		status := 0
		for _, n := range []int{400, 401, 403, 404, 409, 422, 500, 502, 503} {
			if strings.Contains(stderr, fmt.Sprintf("HTTP %d", n)) {
				status = n
			}
		}
		return nil, &Unreadable{Where: where, Detail: "reading " + where + " failed: " + truncate(strings.TrimSpace(stderr), 400), Status: status}
	}
	value, decodeErr := decodeForgeJSON(out)
	if decodeErr != nil {
		return nil, &Unreadable{Where: where, Detail: "reading " + where + " returned something that is not JSON"}
	}
	return value, nil
}
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}
func (f *Forge) Rest(path, where string, params map[string]string) (any, error) {
	if len(params) > 0 {
		q := url.Values{}
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			q.Set(k, params[k])
		}
		path += "?" + q.Encode()
	}
	return f.execute([]string{"api", "--method", "GET", "-H", "Accept: application/vnd.github+json", path}, where)
}
func (f *Forge) GraphQL(document, where string, variables map[string]any) (map[string]any, error) {
	if strings.Contains(strings.ToLower(document), "mutation") {
		return nil, &ForgeUsage{"this collector issues queries only; the document names a mutation"}
	}
	argv := []string{"api", "graphql", "-f", "query=" + document}
	keys := make([]string, 0, len(variables))
	for k := range variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := variables[k]
		if v == nil {
			continue
		}
		flag := "-f"
		switch v.(type) {
		case int, int64, json.Number, *big.Int, bool:
			flag = "-F"
		}
		argv = append(argv, flag, k+"="+graphqlText(v))
	}
	raw, err := f.execute(argv, where)
	if err != nil {
		return nil, err
	}
	o := forgeObject(raw, true)
	if pyvalue.Truthy(o["errors"]) {
		return nil, &Unreadable{Where: where, Detail: "the forge refused the query for " + where + ": " + truncate(pyjson.Dumps(o["errors"], pyjson.Options{}), 400)}
	}
	return forgeObject(o["data"], true), nil
}

const threadsQuery = "query($owner:String!,$name:String!,$number:Int!,$first:Int!,$after:String){\n  repository(owner:$owner,name:$name){pullRequest(number:$number){\n    reviewThreads(first:$first,after:$after){\n      totalCount pageInfo{hasNextPage endCursor}\n      nodes{id isResolved isOutdated path line originalLine\n        comments(first:1){nodes{url author{login} body createdAt}}}}}}}"
const reviewsQuery = "query($owner:String!,$name:String!,$number:Int!,$first:Int!,$after:String){\n  repository(owner:$owner,name:$name){pullRequest(number:$number){\n    reviews(first:$first,after:$after){\n      totalCount pageInfo{hasNextPage endCursor}\n      nodes{id state url body submittedAt author{login}}}}}}"
const commentsQuery = "query($owner:String!,$name:String!,$number:Int!,$first:Int!,$after:String){\n  repository(owner:$owner,name:$name){pullRequest(number:$number){\n    comments(first:$first,after:$after){\n      totalCount pageInfo{hasNextPage endCursor}\n      nodes{id url body createdAt author{login}}}}}}"

// These spell the collector's Python access expressions, not Go type assertions.
func mapOf(v any) map[string]any { return forgeObject(v, true) }
func listOf(v any) []any         { return forgeItems(v) }
func integer(v any) int          { return int(Integer(v).Int64()) }
func graphqlText(v any) string {
	if b, ok := v.(bool); ok {
		return strconv.FormatBool(b)
	}
	return pyvalue.Str(v)
}
func boolOf(v any) bool  { return pyvalue.Truthy(v) }
func strOf(v any) string { return forgeText(v) }
func idOf(v any) any     { return forgeObject(v, false)["id"] }

func (f *Forge) enumerateREST(name, path, key string, params map[string]string, identify func(any) any) (Enumeration, error) {
	return EnumerateConnection(name, f.PageBudget, func(token any) (Page, error) {
		page := integer(Or(token, 1))
		ordered := path + "?page=" + strconv.Itoa(page) + "&per_page=" + pyvalue.Str(f.PageSize)
		for _, key := range []string{"head_sha", "filter"} {
			if value, ok := params[key]; ok {
				ordered += "&" + key + "=" + url.QueryEscape(value)
			}
		}
		raw, err := f.Rest(ordered, name, nil)
		if err != nil {
			return Page{}, err
		}
		o := mapOf(raw)
		items := collectionItems(o[key])
		total := o["total_count"]
		var next any
		if countReached(len(items), f.PageSize) {
			next = strconv.Itoa(page + 1)
		}
		return Page{items, total, next}, nil
	}, identify)
}
func (f *Forge) enumerateArray(name, path string) (Enumeration, error) {
	return EnumerateConnection(name, f.PageBudget, func(token any) (Page, error) {
		page := integer(Or(token, 1))
		raw, err := f.Rest(path, name, map[string]string{"page": strconv.Itoa(page), "per_page": pyvalue.Str(f.PageSize)})
		if err != nil {
			return Page{}, err
		}
		items, ok := raw.([]any)
		if raw == nil {
			items, ok = []any{}, true
		}
		if !ok {
			return Page{}, &Unreadable{Where: name, Detail: "the the " + name + " endpoint answered with " + quote.Kind(raw) + " where a list was expected"}
		}
		var next any
		if countReached(len(items), f.PageSize) {
			next = strconv.Itoa(page + 1)
		}
		return Page{items, nil, next}, nil
	}, func(any) any { return nil })
}
func (f *Forge) enumerateGraphQL(label, document, field, owner, name string, number any) (Enumeration, error) {
	return EnumerateConnection(label, f.PageBudget, func(token any) (Page, error) {
		data, err := f.GraphQL(document, label, map[string]any{"owner": owner, "name": name, "number": number, "first": f.PageSize, "after": token})
		if err != nil {
			return Page{}, err
		}
		node := mapOf(mapOf(mapOf(data["repository"])["pullRequest"])[field])
		info := mapOf(node["pageInfo"])
		var next any
		if boolOf(info["hasNextPage"]) {
			next = info["endCursor"]
			if !pyvalue.Truthy(next) {
				return Page{}, &Unreadable{Where: label, Detail: "the " + label + " connection says another page exists and gives no cursor to reach it"}
			}
		}
		return Page{collectionItems(node["nodes"]), node["totalCount"], next}, nil
	}, idOf)
}
func readEnumeration(found Enumeration, err error, problems *[]Problem) Enumeration {
	if err != nil {
		// forge._read replaces a failed connection, rather than publishing its prefix.
		found = Enumeration{Name: found.Name}
		var unread *Unreadable
		if errors.As(err, &unread) {
			found.Problems = append(found.Problems, Problem{Code: UnreadableCode, Detail: unread.Detail})
		} else {
			found.Problems = append(found.Problems, Problem{Code: UnreadableCode, Detail: err.Error()})
		}
	}
	*problems = append(*problems, found.Problems...)
	return found
}

func candidate(f *Forge, owner, name string, number any) (map[string]any, error) {
	raw, err := f.Rest(fmt.Sprintf("repos/%s/%s/pulls/%s", owner, name, pyvalue.Str(number)), "the pull request", nil)
	if err != nil {
		return nil, err
	}
	p := mapOf(raw)
	head, base := mapOf(p["head"]), mapOf(p["base"])
	return map[string]any{"number": p["number"], "url": p["html_url"], "state": p["state"], "merged": boolOf(p["merged"]), "isDraft": boolOf(p["draft"]), "headSha": head["sha"], "baseSha": base["sha"], "baseRef": base["ref"], "mergeable": p["mergeable"], "mergeStateStatus": strings.ToUpper(defaultString(p["mergeable_state"], "unknown"))}, nil
}
func defaultString(v any, d string) string {
	s := strOf(v)
	if s == "" {
		return d
	}
	return s
}
func threadFinding(raw any) map[string]any {
	n := forgeObject(raw, false)
	comments := Or(mapOf(n["comments"])["nodes"], []any{map[string]any{}})
	c := forgeObject(Index(comments, 0), false)
	line := n["line"]
	if line == nil {
		line = n["originalLine"]
	}
	author := Or(mapOf(c["author"])["login"], "")
	excerpt := strings.Join(strings.Fields(strOf(c["body"])), " ")
	return map[string]any{"kind": "reviewThread", "id": n["id"], "resolved": boolOf(n["isResolved"]), "outdated": boolOf(n["isOutdated"]), "path": n["path"], "line": line, "author": author, "url": c["url"], "excerpt": truncate(excerpt, 400)}
}

func collectReview(f *Forge, owner, name string, number any, problems *[]Problem, connections *[]any) (map[string]any, []any) {
	first, err := f.enumerateGraphQL("review threads", threadsQuery, "reviewThreads", owner, name, number)
	first = readEnumeration(first, err, problems)
	*connections = append(*connections, first.Record())
	findings := make([]any, len(first.Items))
	unresolved := 0
	before := map[string]bool{}
	identifiers := map[string]any{}
	for i, item := range first.Items {
		finding := threadFinding(item)
		findings[i] = finding
		resolved := boolOf(finding["resolved"])
		if pyvalue.Truthy(finding["id"]) {
			key := HashKey(finding["id"])
			before[key] = resolved
			identifiers[key] = finding["id"]
		}
		if !resolved {
			unresolved++
		}
	}
	if first.Total == nil && first.Complete {
		*problems = append(*problems, Problem{Code: EnumerationCountDisagrees, Detail: "the review thread connection did not report a total, so there is nothing to compare the threads that were read against"})
	}
	if first.Complete && len(first.Problems) == 0 && unresolved == 0 {
		second, e := f.enumerateGraphQL("review threads (confirming pass)", threadsQuery, "reviewThreads", owner, name, number)
		second = readEnumeration(second, e, problems)
		*connections = append(*connections, second.Record())
		if second.Complete && len(second.Problems) == 0 {
			after := map[string]bool{}
			for _, item := range second.Items {
				n := forgeObject(item, false)
				after[HashKey(n["id"])] = boolOf(n["isResolved"])
			}
			if pyjson.Dumps(before, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) != pyjson.Dumps(after, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) {
				*problems = append(*problems, Problem{Code: ReviewSetUnstable, Detail: "the review threads were read twice and the two readings disagree about which threads exist or which are resolved, so no stable set of them was observed and the zero this would have reported is not a count"})
			}
		}
	}
	seen := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		seen = append(seen, pyvalue.Str(id))
	}
	sort.Strings(seen)
	var total any = len(seen)
	if first.Total != nil {
		total = first.Total
	}
	return map[string]any{"hasNextPage": !first.Complete, "pagesRead": len(first.Pages), "totalCount": total, "threadsSeen": seen, "unresolved": unresolved}, findings
}

func collectDiscussion(f *Forge, owner, name string, number any, problems *[]Problem, connections *[]any) ([]any, []any) {
	var findings, reviews []any
	for _, spec := range []struct{ label, query, field, kind string }{{"submitted reviews", reviewsQuery, "reviews", "review"}, {"summary comments", commentsQuery, "comments", "comment"}} {
		found, err := f.enumerateGraphQL(spec.label, spec.query, spec.field, owner, name, number)
		found = readEnumeration(found, err, problems)
		*connections = append(*connections, found.Record())
		for _, raw := range found.Items {
			n := forgeObject(raw, false)
			entry := map[string]any{"kind": spec.kind, "id": n["id"], "author": Or(mapOf(n["author"])["login"], ""), "url": n["url"], "excerpt": truncate(strings.Join(strings.Fields(strOf(n["body"])), " "), 400)}
			if spec.kind == "review" {
				entry["state"] = n["state"]
				entry["submittedAt"] = n["submittedAt"]
				reviews = append(reviews, entry)
			} else {
				entry["createdAt"] = n["createdAt"]
			}
			findings = append(findings, entry)
		}
	}
	return findings, reviews
}

func outcome(n map[string]any) string {
	if s, ok := n["conclusion"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	if s, ok := n["status"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return "unknown"
}
func provider(n map[string]any) any {
	id := mapOf(n["app"])["id"]
	if id == nil {
		return nil
	}
	return pyvalue.Str(id)
}
func collectChecks(f *Forge, owner, name, head string, problems *[]Problem, connections *[]any) ([]any, []any, []any) {
	root := "repos/" + owner + "/" + name
	runs, err := f.enumerateREST("workflow runs", root+"/actions/runs", "workflow_runs", map[string]string{"head_sha": head}, idOf)
	runs = readEnumeration(runs, err, problems)
	*connections = append(*connections, runs.Record())
	type lane struct {
		stamp string
		id    *big.Int
	}
	laneKey := func(r map[string]any) [2]string { return [2]string{HashKey(r["workflow_id"]), HashKey(r["event"])} }
	newest := map[[2]string]lane{}
	for _, raw := range runs.Items {
		r := mapOf(raw)
		id, valid := forgeRunID(r["id"])
		if !valid {
			continue
		}
		key := laneKey(r)
		stamp := defaultString(r["run_started_at"], strOf(r["created_at"]))
		if old, ok := newest[key]; !ok || stamp > old.stamp || stamp == old.stamp && id.Cmp(old.id) > 0 {
			newest[key] = lane{stamp, id}
		}
	}
	var entries, detail, superseded []any
	jobIDs := map[string]bool{}
	type binding struct {
		entry map[string]any
		id    string
	}
	var bindings []binding
	for _, raw := range runs.Items {
		r := mapOf(raw)
		runID, valid := forgeRunID(r["id"])
		if !valid {
			*problems = append(*problems, Problem{Code: UnreadableCode, Detail: "a workflow run came back without a usable id, so its jobs cannot be read"})
			continue
		}
		runText := runID.String()
		key := laneKey(r)
		replaced := newest[key].id.Cmp(runID) != 0 && strings.EqualFold(strOf(r["conclusion"]), "cancelled")
		if replaced {
			superseded = append(superseded, map[string]any{"runId": runText, "workflowId": r["workflow_id"], "event": r["event"], "url": r["html_url"], "conclusion": r["conclusion"]})
		}
		label := "jobs of workflow run " + runText
		jobs, e := f.enumerateREST(label, root+"/actions/runs/"+runText+"/jobs", "jobs", map[string]string{"filter": "all"}, idOf)
		jobs = readEnumeration(jobs, e, problems)
		*connections = append(*connections, jobs.Record())
		type jobKey struct {
			item        map[string]any
			name        string
			attempt, id *big.Int
		}
		ordered := make([]jobKey, 0, len(jobs.Items))
		for _, raw := range jobs.Items {
			j := forgeObject(raw, false)
			ordered = append(ordered, jobKey{j, strOf(j["name"]), Integer(Or(j["run_attempt"], 1)), Integer(Or(j["id"], 0))})
		}
		sort.SliceStable(ordered, func(i, j int) bool {
			a, b := ordered[i], ordered[j]
			if a.name != b.name {
				return a.name < b.name
			}
			if n := a.attempt.Cmp(b.attempt); n != 0 {
				return n < 0
			}
			return a.id.Cmp(b.id) < 0
		})
		positions := map[string]int{}
		for _, jr := range ordered {
			j := jr.item
			jid := HashKey(j["id"])
			jobIDs[jid] = true
			attempt := json.Number(Integer(Or(j["run_attempt"], 1)).String())
			slot := strOf(j["name"]) + "|" + string(attempt)
			index := positions[slot]
			positions[slot]++
			identity := fmt.Sprintf("workflow-run:%s:%s#%d", runText, strOf(j["name"]), index)
			entry := map[string]any{"runId": identity, "name": strOf(j["name"]), "headSha": strOf(r["head_sha"]), "conclusion": outcome(j), "attempt": attempt, "provider": nil}
			if !replaced {
				entries = append(entries, entry)
				bindings = append(bindings, binding{entry, jid})
			}
			detail = append(detail, map[string]any{"source": "workflow-job", "runId": identity, "name": strOf(j["name"]), "superseded": replaced, "status": j["status"], "conclusion": j["conclusion"], "attempt": attempt, "startedAt": j["started_at"], "completedAt": j["completed_at"], "url": j["html_url"], "workflowRunUrl": r["html_url"], "workflowName": r["name"]})
		}
	}
	published, e := f.enumerateREST("check runs", root+"/commits/"+head+"/check-runs", "check_runs", map[string]string{"filter": "latest"}, idOf)
	published = readEnumeration(published, e, problems)
	*connections = append(*connections, published.Record())
	providers := map[string]any{}
	for _, raw := range published.Items {
		n := mapOf(raw)
		providers[HashKey(n["id"])] = provider(n)
	}
	for _, b := range bindings {
		b.entry["provider"] = providers[b.id]
	}
	for _, raw := range published.Items {
		n := mapOf(raw)
		id := pyvalue.Str(n["id"])
		if jobIDs[HashKey(n["id"])] {
			continue
		}
		rid := "check-run:" + id
		entry := map[string]any{"runId": rid, "name": strOf(n["name"]), "headSha": strOf(n["head_sha"]), "conclusion": outcome(n), "attempt": 1, "provider": provider(n)}
		entries = append(entries, entry)
		detail = append(detail, map[string]any{"source": "check-run", "runId": rid, "name": strOf(n["name"]), "superseded": false, "status": n["status"], "conclusion": n["conclusion"], "attempt": 1, "startedAt": n["started_at"], "completedAt": n["completed_at"], "url": n["html_url"], "app": mapOf(n["app"])["slug"], "provider": provider(n)})
	}
	statuses, e := f.enumerateREST("commit statuses", root+"/commits/"+head+"/status", "statuses", nil, func(v any) any { return forgeObject(v, false)["context"] })
	statuses = readEnumeration(statuses, e, problems)
	*connections = append(*connections, statuses.Record())
	for _, raw := range statuses.Items {
		n := mapOf(raw)
		context := strOf(n["context"])
		entries = append(entries, map[string]any{"runId": "status:" + context, "name": context, "headSha": head, "conclusion": defaultString(n["state"], "unknown"), "attempt": 1, "provider": nil})
		detail = append(detail, map[string]any{"source": "commit-status", "runId": "status:" + context, "name": context, "superseded": false, "status": n["state"], "conclusion": n["state"], "attempt": 1, "url": n["target_url"], "updatedAt": n["updated_at"]})
	}
	return entries, detail, superseded
}

func collectGates(f *Forge, owner, name string, base any, problems *[]Problem, connections *[]any) map[string]any {
	g := map[string]any{"readable": false, "baseRefExists": nil, "requiredDeclared": nil, "requiredProviders": map[string]any{}, "strictBase": false, "threadResolutionRequired": nil, "threadResolutionNote": "recorded for the reader; the handoff refuses an unresolved thread regardless, so this cannot loosen the gate", "digest": nil}
	ref, err := BranchRef(base)
	if err != nil {
		*problems = append(*problems, Problem{Code: UnreadableCode, Detail: err.Error()})
		return g
	}
	escaped := strings.Split(ref, "/")
	for i, part := range escaped {
		escaped[i] = strings.ReplaceAll(url.QueryEscape(part), "+", "%20")
	}
	refPath := strings.Join(escaped, "/")
	root := "repos/" + owner + "/" + name
	if _, err = f.Rest(root+"/git/ref/heads/"+refPath, "the base branch", nil); err != nil {
		var unread *Unreadable
		if errors.As(err, &unread) && unread.Status == 404 {
			g["baseRefExists"] = false
			*problems = append(*problems, Problem{Code: BaseRefMissing, Detail: "the base branch " + quote.Value(ref) + " does not exist, so this candidate has no destination and its gates cannot be read from one"})
		} else {
			*problems = append(*problems, Problem{Code: UnreadableCode, Detail: err.Error()})
		}
		return g
	}
	g["baseRefExists"] = true
	rules, e := f.enumerateArray("effective branch rules", root+"/rules/branches/"+refPath)
	if e != nil {
		detail := e.Error()
		detail = strings.Replace(detail, "reading effective branch rules", "reading the effective branch rules", 1)
		*problems = append(*problems, Problem{Code: UnreadableCode, Detail: detail})
		return g
	}
	rules.Identifiers = make([]any, len(rules.Items))
	for i := range rules.Items {
		rules.Identifiers[i] = strconv.Itoa(i)
	}
	if connections != nil {
		*connections = append(*connections, rules.Record())
	}
	truncating := false
	for _, p := range rules.Problems {
		if p.Code != EnumerationDuplicated {
			*problems = append(*problems, p)
			truncating = true
		}
	}
	if !rules.Complete || truncating {
		return g
	}
	var required []string
	providers := map[string][]string{}
	var providerOrder []string
	for _, raw := range rules.Items {
		if _, ok := Object(raw); !ok {
			continue
		}
		r := mapOf(raw)
		ruleType, _ := r["type"].(string)
		switch ruleType {
		case "required_status_checks":
			params := mapOf(r["parameters"])
			if boolOf(params["strict_required_status_checks_policy"]) {
				g["strictBase"] = true
			}
			for _, one := range listOf(params["required_status_checks"]) {
				x := mapOf(one)
				context := strings.TrimSpace(strOf(x["context"]))
				if context != "" {
					required = append(required, context)
					if x["integration_id"] != nil {
						if _, exists := providers[context]; !exists {
							providerOrder = append(providerOrder, context)
						}
						providers[context] = append(providers[context], pyvalue.Str(x["integration_id"]))
					}
				}
			}
		case "pull_request":
			g["threadResolutionRequired"] = boolOf(mapOf(r["parameters"])["required_review_thread_resolution"])
		}
	}
	sort.Strings(required)
	required = slices.Compact(required)
	providerAny := contract.OrderedObject{}
	for _, k := range providerOrder {
		v := providers[k]
		sort.Strings(v)
		providerAny = append(providerAny, contract.Field{Key: k, Value: listOf(slices.Compact(v))})
	}
	g["readable"] = true
	g["requiredDeclared"] = required
	g["requiredProviders"] = providerAny
	digestInput := map[string]any{"required": required, "providers": providerAny, "strictBase": g["strictBase"], "threadResolutionRequired": g["threadResolutionRequired"]}
	sum := pyvalue.SHA256Hex(pyjson.Dumps(digestInput, pyjson.Options{SortKeys: true}))
	g["digest"] = sum
	return g
}

func problemsJSON(problems []Problem) []any {
	out := make([]any, len(problems))
	for i, p := range problems {
		out[i] = map[string]any{"code": p.Code, "detail": p.Detail}
	}
	return out
}
func snapshotBase(f *Forge, owner, name string, number any, started string, problems []Problem, connections []any, pinned, reread, coverage map[string]any, findings, checks, detail, superseded []any, gates map[string]any) map[string]any {
	if pinned == nil {
		pinned = map[string]any{}
	}
	if gates == nil {
		gates = map[string]any{"readable": false, "requiredDeclared": nil, "strictBase": false, "baseRefExists": nil, "threadResolutionRequired": nil, "requiredProviders": map[string]any{}, "digest": nil}
	}
	if n, ok := number.(*big.Int); ok {
		number = json.Number(n.String())
	}
	required := gates["requiredDeclared"]
	disabled := []string{"codex", "codex review", "chatgpt-codex-connector", "codex-review"}
	disabledSet := map[string]bool{}
	for _, reviewer := range disabled {
		disabledSet[strings.ToLower(strings.TrimSpace(reviewer))] = true
	}
	conflicting := []any{}
	for _, raw := range listOf(required) {
		name := pyvalue.Str(raw)
		if disabledSet[strings.ToLower(strings.TrimSpace(name))] {
			conflicting = append(conflicting, name)
		}
	}
	if len(conflicting) > 0 {
		problems = append(problems, Problem{Code: RequiredGateConflict, Detail: "this branch declares " + quote.Value(pyvalue.Str(conflicting[0])) + " required, and that reviewer is disabled by policy; it stays in the required set because removing it here would hide a rule that needs an authorised correction"})
	}
	handoff := map[string]any{"isDraft": boolOf(pinned["isDraft"]), "baseVerifiedAt": f.Now(), "baseSha": pinned["baseSha"], "baseRef": pinned["baseRef"], "reviewCoverage": defaultMap(coverage), "checks": defaultList(checks), "requiredDeclared": required, "requiredProviders": gates["requiredProviders"], "threadDispositions": []any{}, "criterionEvidence": []any{}, "limitations": []any{}}
	return map[string]any{"repository": owner + "/" + name, "number": number, "url": pinned["url"], "observation": map[string]any{"startedAt": started, "finishedAt": f.Now(), "atomic": false, "note": "this observation is not atomic with any merge that follows it; the exact-head guard at merge time and the late-finding path afterwards are what bound the window"}, "pinned": pinned, "reread": reread, "handoffGuidance": "this handoff carries the observed half of a completion report: the review coverage, the checks, the required gates, the draft flag and the base it was verified against. threadDispositions, criterionEvidence and limitations are left empty because they are judgements rather than observations; a candidate whose threads were seen needs a judged disposition for each of them before the report is submitted", "gates": gates, "supersededRuns": defaultList(superseded), "connections": defaultList(connections), "handoff": handoff, "findings": defaultList(findings), "checkDetail": defaultList(detail), "problems": problemsJSON(problems), "verdict": VerdictOf(problems), "provenance": map[string]any{"calls": f.Calls, "disabledReviewers": disabled, "conflictingRequiredReviewers": conflicting}}
}
func defaultMap(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
func defaultList(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func Collect(f *Forge, repository string, numberValue any) (snapshot map[string]any, err error) {
	defer func() {
		if value := recover(); value != nil {
			if failure, ok := value.(*PythonError); ok {
				snapshot, err = nil, failure
			} else {
				panic(value)
			}
		}
	}()
	owner, name, err := SplitRepository(repository)
	if err != nil {
		return nil, err
	}
	number, err := PullRequestNumber(numberValue)
	if err != nil {
		return nil, err
	}
	started := f.Now()
	var problems []Problem
	var connections []any
	pinned, err := candidate(f, owner, name, number)
	if err != nil {
		var unread *Unreadable
		if !errors.As(err, &unread) {
			return nil, err
		}
		problems = append(problems, Problem{Code: UnreadableCode, Detail: err.Error()})
		return snapshotBase(f, owner, name, number, started, problems, connections, nil, nil, nil, nil, nil, nil, nil, nil), nil
	}
	head := strOf(pinned["headSha"])
	if !shaPattern.MatchString(head) {
		problems = append(problems, Problem{Code: UnreadableCode, Detail: "the pull request did not report a head commit, so there is nothing to collect evidence about"})
		return snapshotBase(f, owner, name, number, started, problems, connections, pinned, nil, nil, nil, nil, nil, nil, nil), nil
	}
	coverage, findings := collectReview(f, owner, name, number, &problems, &connections)
	discussion, reviews := collectDiscussion(f, owner, name, number, &problems, &connections)
	findings = append(findings, discussion...)
	checks, detail, superseded := collectChecks(f, owner, name, head, &problems, &connections)
	gates := collectGates(f, owner, name, pinned["baseRef"], &problems, &connections)
	after, e := candidate(f, owner, name, number)
	var reread map[string]any
	graded := pinned
	if e != nil {
		problems = append(problems, Problem{Code: UnreadableCode, Detail: "the pull request could not be re-read to confirm it had not moved: " + e.Error()})
	} else {
		reread = after
		reread["verifiedAt"] = f.Now()
		var moved []string
		for _, field := range []string{"headSha", "baseSha", "baseRef", "state", "merged", "isDraft"} {
			if !pyvalue.ItemEqual(after[field], pinned[field]) {
				moved = append(moved, field+": "+quote.Value(pinned[field])+" to "+quote.Value(after[field]))
			}
		}
		if len(moved) > 0 {
			problems = append(problems, Problem{Code: CandidateMoved, Detail: "the candidate changed while this was being collected (" + strings.Join(moved, ", ") + "), so what was read describes something that is no longer the candidate"})
		} else {
			graded = after
		}
	}
	if gates["digest"] != nil {
		var confirming []Problem
		again := collectGates(f, owner, name, pinned["baseRef"], &confirming, nil)
		problems = append(problems, confirming...)
		if again["digest"] != nil && again["digest"] != gates["digest"] {
			problems = append(problems, Problem{Code: GatesMoved, Detail: "the branch's effective rules changed while this was being collected, so the checks that were read were graded against gates that are no longer the ones this branch declares"})
		}
	}
	problems = append(problems, CandidateProblems(graded, boolOf(gates["strictBase"]))...)
	requiredAny := gates["requiredDeclared"]
	providerMap := ProviderMap(gates["requiredProviders"])
	problems = append(problems, HandoffProblems(head, coverage, checks, requiredAny, providerMap, reviews)...)
	return snapshotBase(f, owner, name, number, started, problems, connections, pinned, reread, coverage, findings, checks, detail, superseded, gates), nil
}

func RestateProblems(head string, record, snapshot any) []Problem {
	_, ok := Object(record)
	if !ok {
		return []Problem{{Code: Malformed, Detail: "a handoff record is an object, not " + quote.Kind(record)}}
	}
	r := mapOf(record)
	snap := mapOf(snapshot)
	pinned := mapOf(snap["pinned"])
	providers := r["requiredProviders"]
	var problems []Problem
	if providers != nil {
		if _, ok := Object(providers); !ok {
			problems = append(problems, Problem{Code: RecordInvalid, Detail: "the record states requiredProviders as " + quote.Kind(providers) + ", not a mapping of context to the integration its rule names"})
			providers = nil
		}
	}
	required := r["requiredDeclared"]
	checks := r["checks"]
	if !pyvalue.Truthy(checks) {
		checks = []any{}
	}
	// Restated checks are shape-validated, not iterated like a forge collection.
	problems = append(problems, HandoffProblems(head, r["reviewCoverage"], checks, required, ProviderMap(providers), nil)...)
	for _, p := range problems {
		if p.Code == Malformed {
			return problems
		}
	}
	gates := mapOf(snap["gates"])
	fresh, stated := listOf(gates["requiredDeclared"]), listOf(required)
	if gates["requiredDeclared"] != nil && required != nil && pyvalue.Repr(sortedTexts(fresh)) != pyvalue.Repr(sortedTexts(stated)) {
		problems = append(problems, Problem{Code: GatesMoved, Detail: "the record was graded against required checks " + quote.Value(sortedTexts(stated)) + " and this branch now declares " + quote.Value(sortedTexts(fresh))})
	}
	expected, recorded := providerObject(gates["requiredProviders"]), providerObject(providers)
	if _, ok := Object(gates["requiredProviders"]); ok && pyjson.Dumps(expected, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) != pyjson.Dumps(recorded, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) {
		said := "nothing"
		if len(recorded) > 0 {
			said = quote.Value(recorded)
		}
		problems = append(problems, Problem{Code: GatesMoved, Detail: "the record states " + said + " about which integration answers for a required context and this branch declares " + quote.Value(expected)})
	}
	observed := strOf(pinned["headSha"])
	if head != "" && observed != "" && head != observed {
		problems = append(problems, Problem{Code: CandidateMoved, Detail: "the record is about head " + quote.Value(head) + " and the forge now reports " + quote.Value(observed) + ", so the record describes a commit that is no longer the candidate"})
	}
	base := strOf(r["baseSha"])
	if base != "" && strOf(pinned["baseSha"]) != "" && base != strOf(pinned["baseSha"]) {
		problems = append(problems, Problem{Code: CandidateMoved, Detail: "the record was verified against base " + quote.Value(base) + " and the candidate now targets " + quote.Value(strOf(pinned["baseSha"])) + ", so its checks cover a merge that is no longer the one being made"})
	} else if base == "" {
		problems = append(problems, Problem{Code: RecordInvalid, Detail: "the record does not name the base commit it was verified against, so a destination that moved under an unchanged head cannot be noticed"})
	}
	for _, raw := range listOf(snap["problems"]) {
		one := mapOf(raw)
		code, exists := one["code"]
		if !exists {
			code = UnreadableCode
		}
		problems = append(problems, Problem{Code: strOf(code), Detail: strOf(one["detail"])})
	}
	seen := map[string]bool{}
	for _, id := range listOf(mapOf(r["reviewCoverage"])["threadsSeen"]) {
		seen[pyvalue.Str(id)] = true
	}
	var late []string
	for _, raw := range listOf(snap["findings"]) {
		one := mapOf(raw)
		if one["kind"] == "reviewThread" && !seen[pyvalue.Str(one["id"])] {
			label := strOf(one["url"])
			if label == "" {
				label = pyvalue.Str(one["id"])
			}
			late = append(late, label)
		}
	}
	if len(late) > 0 {
		sort.Strings(late)
		problems = append(problems, Problem{Code: LateFinding, Detail: fmt.Sprintf("%d review thread(s) on this head are not in the record's threadsSeen, so the record did not see them and no longer describes the candidate: %s", len(late), strings.Join(late[:min(5, len(late))], ", "))})
	}
	return problems
}
func sortedTexts(v []any) []string {
	out := make([]string, len(v))
	for i, x := range v {
		out[i] = pyvalue.Str(x)
	}
	sort.Strings(out)
	return out
}
