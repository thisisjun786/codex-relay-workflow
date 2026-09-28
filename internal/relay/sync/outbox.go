package sync

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const CoordinationDocument = "coordination_document"

// Outbox never performs a remote write. The caller executes Operation's conditional replacement.
type Outbox struct {
	Store *store.Store
	Clock delivery.Clock
	Token func() (string, error)
}

func New(s *store.Store, c delivery.Clock) *Outbox {
	return &Outbox{Store: s, Clock: c, Token: func() (string, error) { b := make([]byte, 16); _, e := rand.Read(b); return hex.EncodeToString(b), e }}
}
func refuse(reason, format string, args ...any) error {
	return &store.RefusedError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}
func rowObject(r store.Row) Obj {
	o := make(Obj, 0, len(r))
	for _, c := range r {
		o = append(o, contract.Field{Key: c.Name, Value: c.Value})
	}
	return o
}
func (s *Outbox) Get(ctx context.Context, id string) (store.Row, error) {
	r, e := s.Store.One(ctx, "SELECT * FROM sync_outbox WHERE sync_id = ?", id)
	if e == nil && r == nil {
		e = refuse("sync_not_claimable", "no synchronisation job %s", store.PyRepr(id))
	}
	return r, e
}
func (s *Outbox) SetTarget(ctx context.Context, rid, target, ref string) (Obj, error) {
	e := s.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return s.Store.SetSyncTarget(ctx, rid, target, ref, s.Clock.ISO())
	})
	return obj("relationshipId", rid, "target", target, "targetRef", ref), e
}
func (s *Outbox) journal(ctx context.Context, kind, id string, detail Obj, at string) error {
	_, e := s.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, kind, id, evidence.Dumps(detail, false, false, true))
	return e
}

// Enqueue holds the nullable identity components separately from the target document.
type Enqueue struct {
	RelationshipID, IssueKey, SubjectKind, Summary, Target         string
	EventID, Generation, Revision, Verdict, CriteriaDigest, Ruling any
}

func IdentityDigest(target, ref, kind, rid string, event, generation, revision, verdict, criteria, ruling any) string {
	parts := []string{target, ref, kind, rid}
	for _, v := range []any{event, generation, revision, verdict} {
		if v == nil {
			parts = append(parts, "null")
		} else {
			parts = append(parts, text(v))
		}
	}
	payload := strings.Join(parts, "|")
	for _, p := range []struct {
		k string
		v any
	}{{"criteria", criteria}, {"ruling", ruling}} {
		if p.v != nil {
			payload += "|" + p.k + "=" + text(p.v)
		}
	}
	return hash(payload)
}
func normalizeGeneration(value any) (any, sql.NullInt64, error) {
	if value == nil {
		return nil, sql.NullInt64{}, nil
	}
	var canonical any = value
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			f, err := v.Float64()
			if err != nil {
				return nil, sql.NullInt64{}, err
			}
			canonical = evidence.Text(f)
		} else {
			canonical = string(v)
		}
	case float64:
		canonical = evidence.Text(v)
	case bool:
		if v {
			return "True", sql.NullInt64{Int64: 1, Valid: true}, nil
		}
		return "False", sql.NullInt64{Int64: 0, Valid: true}, nil
	}
	textValue := text(canonical)
	if i, ok := new(big.Int).SetString(textValue, 10); ok {
		if !i.IsInt64() {
			return nil, sql.NullInt64{}, fmt.Errorf("python int too large to convert to SQLite INTEGER")
		}
		return canonical, sql.NullInt64{Int64: i.Int64(), Valid: true}, nil
	}
	f, err := strconv.ParseFloat(textValue, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < math.MinInt64 || f >= math.MaxInt64 {
		return nil, sql.NullInt64{}, fmt.Errorf("cannot store generation %s as SQLite INTEGER", evidence.Repr(value))
	}
	return canonical, sql.NullInt64{Int64: int64(f), Valid: true}, nil
}

func (s *Outbox) EnqueueIn(ctx context.Context, e Enqueue) (any, error) {
	target := e.Target
	if target == "" {
		target = CoordinationDocument
	}
	configured, err := s.Store.SyncTarget(ctx, e.RelationshipID, target)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	generation, gen, err := normalizeGeneration(e.Generation)
	if err != nil {
		return nil, err
	}
	digest := IdentityDigest(target, configured.TargetRef, e.SubjectKind, e.RelationshipID, e.EventID, generation, e.Revision, e.Verdict, e.CriteriaDigest, e.Ruling)
	id := digest[:32]
	now := s.Clock.ISO()
	ns := func(v any) sql.NullString { return sql.NullString{String: text(v), Valid: v != nil} }
	inserted, err := s.Store.EnqueueSync(ctx, store.SyncOutboxRow{SyncID: id, RelationshipID: e.RelationshipID, IssueKey: e.IssueKey, Target: target, TargetRef: configured.TargetRef, SubjectKind: e.SubjectKind, EventID: ns(e.EventID), ExecutionGeneration: gen, RevisionHash: ns(e.Revision), Verdict: ns(e.Verdict), IdentityDigest: digest, Summary: e.Summary, State: "pending", CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return nil, err
	}
	err = s.journal(ctx, "sync_enqueued", id, obj("subjectKind", e.SubjectKind, "eventId", e.EventID, "criteriaDigest", e.CriteriaDigest, "ruling", e.Ruling, "inserted", inserted), now)
	return id, err
}
func (s *Outbox) Next(ctx context.Context, target string, limit int, now float64) ([]any, error) {
	rows, e := s.Store.NextSyncJobs(ctx, [3]string{"pending", "written", "claimed"}, target, now, limit)
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, r := range rows {
		row, e := s.Get(ctx, r.SyncID)
		if e != nil {
			return nil, e
		}
		out = append(out, rowObject(row))
	}
	return out, nil
}
func (s *Outbox) Claim(ctx context.Context, id, owner string, now float64) (Obj, error) {
	token, e := s.Token()
	if e != nil {
		return nil, e
	}
	e = s.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := s.Get(ctx, id)
		if e != nil {
			return e
		}
		switch text(r.Get("state")) {
		case "confirmed":
			return refuse("sync_not_claimable", "%s is already confirmed", store.PyRepr(id))
		case "failed":
			return refuse("sync_not_claimable", "%s is failed after %v attempts; call retry to resume it deliberately", store.PyRepr(id), r.Get("attempts"))
		}
		if n, ok := r.Get("next_attempt_at").(float64); ok && n > now {
			return refuse("sync_not_claimable", "%s is backing off until %s", store.PyRepr(id), evidence.Dumps(n, false, false, true))
		}
		if n, ok := r.Get("lease_until").(float64); ok && n > now {
			return refuse("sync_not_claimable", "%s is leased by %s until %s", store.PyRepr(id), store.PyRepr(text(r.Get("lease_owner"))), evidence.Dumps(n, false, false, true))
		}
		return s.Store.ClaimSync(ctx, id, "claimed", owner, now+300, token, s.Clock.ISO())
	})
	return obj("syncId", id, "claimToken", token, "owner", owner, "leaseUntil", now+300), e
}
func (s *Outbox) fenced(ctx context.Context, id, token string) (store.Row, error) {
	r, e := s.Get(ctx, id)
	if e == nil && r.Get("claim_token") != token {
		e = refuse("sync_not_claimable", "this claim token is not the one currently held for %s", store.PyRepr(id))
	}
	return r, e
}
func (s *Outbox) Operation(ctx context.Context, id string) (Obj, error) {
	r, e := s.Get(ctx, id)
	if e != nil {
		return nil, e
	}
	rid := text(r.Get("relationship_id"))
	return obj("syncId", id, "target", r.Get("target"), "targetRef", r.Get("target_ref"), "issueKey", r.Get("issue_key"), "subjectKind", r.Get("subject_kind"), "identity", obj("eventId", r.Get("event_id"), "executionGeneration", r.Get("execution_generation"), "revisionHash", r.Get("revision_hash"), "disposition", r.Get("verdict")), "identityDigest", r.Get("identity_digest"), "startMarker", StartMarker(id), "endMarker", EndMarker(id), "containerStartMarker", ContainerStart(rid), "containerEndMarker", ContainerEnd(rid), "block", RenderBlock(r), "protocol", []string{
		"read the target document and locate this relationship's CONTAINER markers",
		"container absent: this is initialisation. Create it once, with an empty body, before any job writes. If that response is lost, do not retry blindly: read again and reconcile, because a negative read is not proof of non-delivery while an earlier write can still land",
		"container present: every write, INCLUDING this job's first block, is a conditional replacement of the container's exact current text with its new text. Never a bare append: an append is unconditional, so a slow writer whose lease expired can still land a second copy after another writer appended",
		"this job's block present with a matching record: the write already landed; complete from that observation and do NOT write again",
		"present with a different record, or duplicated, or unterminated: reconcile reports duplicate or malformed; repair the container in one conditional replacement rather than appending over it",
		"read the document back and pass the full text to complete",
	}, "note", "the connector's document save takes no idempotency key, so the marker read plus conditional replacement of an owned container is what makes a retry safe, not a request id. Local claim fencing cannot retract a remote write that was already issued, which is why insertion is conditional too.", "formatEvidence", "HTML relay-sync markers survive a real Linear document write and readback byte-for-byte; see JUN-92-linear-format-probe.json"), nil
}
func (s *Outbox) Reconcile(ctx context.Context, id, observed string) (Obj, error) {
	r, e := s.Get(ctx, id)
	if e != nil {
		return nil, e
	}
	d := ParseDocument(observed)
	if slices.Contains(d.Malformed, id) {
		return obj("syncId", id, "outcome", "malformed", "detail", "an unterminated block for this job is present; the previous write's effect is uncertain and must not be appended over"), nil
	}
	if slices.Contains(d.Duplicates, id) {
		return obj("syncId", id, "outcome", "duplicate", "detail", "more than one block for this job is present; that is not one successful unique record and must be repaired before confirming"), nil
	}
	b, ok := d.Blocks[id]
	if !ok {
		return obj("syncId", id, "outcome", "absent", "detail", "no block for this job was observed. A negative read is not affirmative non-delivery while an earlier write may still land, so write only through the conditional container replacement"), nil
	}
	if mismatch := PayloadMismatch(r, b); len(mismatch) > 0 {
		return obj("syncId", id, "outcome", "stale", "mismatch", mismatch, "previousBlock", b.Text, "detail", "a block exists but describes something else; replace it"), nil
	}
	return obj("syncId", id, "outcome", "already_written", "previousBlock", b.Text, "detail", "this job's write landed; complete it without writing again"), nil
}
func (s *Outbox) Complete(ctx context.Context, id, token, targetRef, readback string, externalRef *string) (Obj, error) {
	stamp := s.Clock.ISO()
	var problems []string
	err := s.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := s.fenced(ctx, id, token)
		if e != nil {
			return e
		}
		if r.Get("state") == "confirmed" {
			return nil
		}
		if r.Get("target_ref") != targetRef {
			return refuse("sync_target_mismatch", "this job targets %s, not %s; validating the right text in the wrong document validates nothing", store.PyRepr(text(r.Get("target_ref"))), store.PyRepr(targetRef))
		}
		d := ParseDocument(readback)
		b, ok := d.Blocks[id]
		switch {
		case slices.Contains(d.Malformed, id):
			problems = []string{"an unterminated block for this job is present, so what landed is uncertain"}
		case slices.Contains(d.Duplicates, id):
			problems = []string{"more than one block for this job is present, which is not one successful unique record"}
		case !ok:
			problems = []string{"the readback carries no block for this job"}
		default:
			problems = PayloadMismatch(r, b)
		}
		if len(problems) == 0 {
			ref := sql.NullString{}
			if externalRef != nil {
				ref = sql.NullString{String: *externalRef, Valid: true}
			}
			if e = s.Store.ConfirmSync(ctx, id, "confirmed", ref, b.Text, stamp); e != nil {
				return e
			}
			return s.journal(ctx, "sync_confirmed", id, obj("targetRef", targetRef), stamp)
		}
		_, e = s.Store.Q(ctx).ExecContext(ctx, "UPDATE sync_outbox SET state = ?, last_error = ?, lease_owner = NULL, lease_until = NULL, updated_at = ? WHERE sync_id = ? AND claim_token = ? AND state != ?", "written", strings.Join(problems, "; "), stamp, id, token, "confirmed")
		return e
	})
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, refuse("readback_mismatch", "the readback does not carry this job's record: %s", strings.Join(problems, "; "))
	}
	r, e := s.Get(ctx, id)
	return rowObject(r), e
}
func (s *Outbox) Fail(ctx context.Context, id, token, message string, now float64) (Obj, error) {
	stamp := s.Clock.ISO()
	e := s.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := s.fenced(ctx, id, token)
		if e != nil {
			return e
		}
		if r.Get("state") == "confirmed" {
			return refuse("sync_not_claimable", "%s is confirmed; a later failure cannot undo it", store.PyRepr(id))
		}
		attempts := r.Get("attempts").(int64) + 1
		state := "pending"
		if attempts >= 8 {
			state = "failed"
		}
		delay := min(900, 30*math.Pow(2, float64(max(0, attempts-1))))
		if e = s.Store.FailSync(ctx, id, state, attempts, message, now+delay, stamp); e != nil {
			return e
		}
		return s.journal(ctx, "sync_failed", id, obj("attempts", attempts, "state", state), stamp)
	})
	if e != nil {
		return nil, e
	}
	r, e := s.Get(ctx, id)
	return rowObject(r), e
}
func (s *Outbox) Retry(ctx context.Context, id string) (Obj, error) {
	e := s.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return s.Store.RetrySync(ctx, id, "pending", s.Clock.ISO(), "confirmed")
	})
	if e != nil {
		return nil, e
	}
	r, e := s.Get(ctx, id)
	return rowObject(r), e
}
func (s *Outbox) Snapshot(ctx context.Context, rid string) (Obj, error) {
	rows, e := s.Store.SyncSnapshot(ctx, rid)
	if e != nil {
		return nil, e
	}
	jobs := []any{}
	pending, failed, confirmed := 0, 0, 0
	for _, row := range rows {
		r, e := s.Get(ctx, row.SyncID)
		if e != nil {
			return nil, e
		}
		jobs = append(jobs, obj("syncId", r.Get("sync_id"), "target", r.Get("target"), "targetRef", r.Get("target_ref"), "subjectKind", r.Get("subject_kind"), "eventId", r.Get("event_id"), "executionGeneration", r.Get("execution_generation"), "revisionHash", r.Get("revision_hash"), "disposition", r.Get("verdict"), "state", r.Get("state"), "attempts", r.Get("attempts"), "lastError", r.Get("last_error"), "nextAttemptAt", r.Get("next_attempt_at"), "confirmedAt", r.Get("confirmed_at")))
		switch row.State {
		case "pending", "claimed", "written":
			pending++
		case "failed":
			failed++
		case "confirmed":
			confirmed++
		}
	}
	return obj("jobs", jobs, "pending", pending, "failed", failed, "confirmed", confirmed), nil
}
