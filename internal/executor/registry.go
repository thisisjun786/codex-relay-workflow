package executor

import (
	"encoding/json"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

type Acknowledgement struct {
	State        string `json:"state"`
	ExecutionID  string `json:"execution_id"`
	TaskDigest   string `json:"task_digest"`
	ResultDigest string `json:"result_digest"`
}

type Record struct {
	Kind            string           `json:"kind"`
	Admission       *Admission       `json:"admission,omitempty"`
	Grant           *Grant           `json:"grant,omitempty"`
	Result          *Result          `json:"result,omitempty"`
	Acknowledgement *Acknowledgement `json:"acknowledgement,omitempty"`
}

type Ledger interface {
	Transact(func(records []Record, appendRecord func(Record) error) error) error
}

type Registry struct {
	ledger Ledger
	now    func() time.Time
}

func NewRegistry(ledger Ledger, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{ledger: ledger, now: now}
}

func ReferenceOf(a Admission) Reference {
	return Reference{Identity: a.Task.Identity, TaskDigest: a.TaskDigest, Executor: a.Task.Executor}
}

var errNoLedger = errors.New("executor: no ledger")

type ledgerState struct {
	currentByTask map[string]string
	admissionBy   map[string]Admission
	grantBy       map[string]Grant
	finalBy       map[string]Record
	quarantine    map[string]Record
}

func cloneRecord(r Record) (Record, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	var out Record
	if err := json.Unmarshal(raw, &out); err != nil {
		return Record{}, err
	}
	return out, nil
}

func cloneAdmission(a Admission) (Admission, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return Admission{}, err
	}
	var out Admission
	if err := json.Unmarshal(raw, &out); err != nil {
		return Admission{}, err
	}
	return out, nil
}

func cloneResult(r Result) (Result, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return Result{}, err
	}
	var out Result
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, err
	}
	return out, nil
}

func admitStep(st *ledgerState, frozen Admission) error {
	eid := frozen.Task.Identity.ExecutionID
	tid := frozen.Task.Identity.TaskID
	if _, dup := st.admissionBy[eid]; dup {
		return errInvalid(CodeIdentityMismatch, "execution_id", "an execution id is bound to another task")
	}
	if cur, ok := st.currentByTask[tid]; ok {
		old := st.admissionBy[cur]
		oi, ni := old.Task.Identity, frozen.Task.Identity
		if ni.Attempt <= oi.Attempt || ni.AssignmentGeneration <= oi.AssignmentGeneration {
			return errInvalid(CodeStaleAssignment, "identity", "the attempt and generation must both increase")
		}
		if ni.Plan != oi.Plan || ni.Node != oi.Node || frozen.Task.Repository.Forge != old.Task.Repository.Forge || frozen.Task.Repository.ID != old.Task.Repository.ID {
			return errInvalid(CodeIdentityMismatch, "identity", "the task moved plan, node or repository")
		}
	}
	return nil
}

func renewStep(last, g Grant) (same bool, err error) {
	if last.ExecutionID == g.ExecutionID && last.AssignmentGeneration == g.AssignmentGeneration &&
		last.Owner == g.Owner && last.OwnerEpoch == g.OwnerEpoch && last.ExpiresAt.Equal(g.ExpiresAt) {
		return true, nil
	}
	if g.OwnerEpoch < last.OwnerEpoch || (g.OwnerEpoch == last.OwnerEpoch && g.Owner != last.Owner) || g.ExpiresAt.Before(last.ExpiresAt) {
		return false, errInvalid(CodeTaskInvalid, "grant", "the renewal is older than the recorded grant")
	}
	return false, nil
}

func foldRecords(records []Record) (*ledgerState, error) {
	st := &ledgerState{currentByTask: map[string]string{}, admissionBy: map[string]Admission{}, grantBy: map[string]Grant{},
		finalBy: map[string]Record{}, quarantine: map[string]Record{}}
	for _, rec := range records {
		switch rec.Kind {
		case "admitted", "grant_renewed":
			if rec.Admission == nil || rec.Grant == nil || rec.Result != nil || rec.Acknowledgement != nil {
				return nil, errInvalid(CodeTaskInvalid, "ledger", "a grant record without exactly its admission and grant")
			}
			a := *rec.Admission
			if err := ValidateAdmission(a); err != nil {
				return nil, err
			}
			if err := ValidateGrant(a, *rec.Grant, time.Time{}); err != nil {
				return nil, err
			}
			eid := a.Task.Identity.ExecutionID
			tid := a.Task.Identity.TaskID
			if rec.Kind == "admitted" {
				if err := admitStep(st, a); err != nil {
					return nil, err
				}
				st.admissionBy[eid] = a
				st.currentByTask[tid] = eid
			} else {
				existing, ok := st.admissionBy[eid]
				if !ok || existing.TaskDigest != a.TaskDigest || ReferenceOf(existing) != ReferenceOf(a) || st.currentByTask[tid] != eid {
					return nil, errInvalid(CodeStaleAssignment, "ledger", "a grant renewal against an execution that is not current")
				}
				if _, err := renewStep(st.grantBy[eid], *rec.Grant); err != nil {
					return nil, err
				}
			}
			st.grantBy[eid] = *rec.Grant
		case "received", "quarantined":
			if rec.Result == nil || rec.Acknowledgement == nil || rec.Admission != nil || rec.Grant != nil {
				return nil, errInvalid(CodeResultInvalid, "ledger", "a result record without exactly its result and acknowledgement")
			}
			res, ack := *rec.Result, *rec.Acknowledgement
			eid := res.Identity.ExecutionID
			adm, ok := st.admissionBy[eid]
			if !ok || st.currentByTask[adm.Task.Identity.TaskID] != eid {
				return nil, errInvalid(CodeStaleAssignment, "ledger", "a result for an execution that is not current")
			}
			peer := Peer{HostID: adm.Task.Executor.HostID, Incarnation: adm.Task.Executor.Incarnation}
			if err := ValidateResult(adm, res, peer); err != nil {
				return nil, err
			}
			wantState := "received"
			if rec.Kind == "quarantined" {
				wantState = "quarantined"
			}
			if ack.State != wantState || ack.ExecutionID != eid || ack.TaskDigest != adm.TaskDigest || !validDigest(ack.ResultDigest) {
				return nil, errInvalid(CodeResultInvalid, "ledger", "the acknowledgement does not match its result")
			}
			if rec.Kind == "received" {
				if _, dup := st.finalBy[eid]; dup {
					return nil, errInvalid(CodeResultInvalid, "ledger", "a second final result for one execution")
				}
				st.finalBy[eid] = rec
			} else {
				final, ok := st.finalBy[eid]
				if !ok {
					return nil, errInvalid(CodeResultInvalid, "ledger", "a quarantine without a final result")
				}
				if final.Acknowledgement.ResultDigest == ack.ResultDigest {
					return nil, errInvalid(CodeResultConflict, "ledger", "a quarantine of the stored result digest")
				}
				if _, dup := st.quarantine[eid+"\x00"+ack.ResultDigest]; dup {
					return nil, errInvalid(CodeResultInvalid, "ledger", "a duplicate quarantine record")
				}
				st.quarantine[eid+"\x00"+ack.ResultDigest] = rec
			}
		default:
			return nil, errInvalid(CodeTaskInvalid, "ledger", "unknown record kind "+rec.Kind)
		}
	}
	return st, nil
}

func (r *Registry) transact(fn func(*ledgerState, func(Record) error) error) error {
	if r.ledger == nil {
		return errNoLedger
	}
	return r.ledger.Transact(func(records []Record, appendRecord func(Record) error) error {
		st, err := foldRecords(records)
		if err != nil {
			return err
		}
		return fn(st, appendRecord)
	})
}

func (r *Registry) Admit(a Admission, g Grant) (Reference, error) {
	if err := ValidateAdmission(a); err != nil {
		return Reference{}, err
	}
	if err := ValidateGrant(a, g, r.now()); err != nil {
		return Reference{}, err
	}
	frozen, err := Freeze(a.Task)
	if err != nil {
		return Reference{}, err
	}
	var ref Reference
	err = r.transact(func(st *ledgerState, app func(Record) error) error {
		eid := frozen.Task.Identity.ExecutionID
		tid := frozen.Task.Identity.TaskID
		if existing, ok := st.admissionBy[eid]; ok {
			if existing.TaskDigest != frozen.TaskDigest {
				return errInvalid(CodeIdentityMismatch, "execution_id", "an execution id is bound to another task")
			}
			if st.currentByTask[tid] != eid {
				return errInvalid(CodeStaleAssignment, "execution_id", "the execution is not the task's current attempt")
			}
			same, err := renewStep(st.grantBy[eid], g)
			if err != nil {
				return err
			}
			if same {
				ref = ReferenceOf(existing)
				return nil
			}
			ca, err := cloneAdmission(existing)
			if err != nil {
				return err
			}
			cg := g
			if err := app(Record{Kind: "grant_renewed", Admission: &ca, Grant: &cg}); err != nil {
				return err
			}
			ref = ReferenceOf(existing)
			return nil
		}
		if err := admitStep(st, frozen); err != nil {
			return err
		}
		ca, err := cloneAdmission(frozen)
		if err != nil {
			return err
		}
		cg := g
		if err := app(Record{Kind: "admitted", Admission: &ca, Grant: &cg}); err != nil {
			return err
		}
		ref = ReferenceOf(frozen)
		return nil
	})
	return ref, err
}

func (r *Registry) Current(executionID string) (Admission, Grant, error) {
	var a Admission
	var g Grant
	err := r.transact(func(st *ledgerState, _ func(Record) error) error {
		adm, ok := st.admissionBy[executionID]
		if !ok || st.currentByTask[adm.Task.Identity.TaskID] != executionID {
			return errInvalid(CodeStaleAssignment, "execution_id", "the execution is not the task's current attempt")
		}
		ca, err := cloneAdmission(adm)
		if err != nil {
			return err
		}
		a, g = ca, st.grantBy[executionID]
		return nil
	})
	return a, g, err
}

func (r *Registry) currentAdmission(st *ledgerState, ref Reference) (Admission, error) {
	adm, ok := st.admissionBy[ref.Identity.ExecutionID]
	if !ok || adm.TaskDigest != ref.TaskDigest || adm.Task.Identity != ref.Identity || adm.Task.Executor != ref.Executor ||
		st.currentByTask[adm.Task.Identity.TaskID] != ref.Identity.ExecutionID {
		return Admission{}, errInvalid(CodeStaleAssignment, "reference", "the reference is not the task's current admitted execution")
	}
	return adm, nil
}

func (r *Registry) Collect(ref Reference, spool, resultPath string, peer Peer, limits Limits) (Acknowledgement, error) {
	var ack Acknowledgement
	err := r.transact(func(st *ledgerState, app func(Record) error) error {
		adm, err := r.currentAdmission(st, ref)
		if err != nil {
			return err
		}
		if !validID(peer.HostID) || !validID(peer.Incarnation) || peer.HostID != adm.Task.Executor.HostID || peer.Incarnation != adm.Task.Executor.Incarnation {
			return errInvalid(CodeHostMismatch, "peer", "the delivery is not from the admitted host and incarnation")
		}
		if err := checkLimits(limits); err != nil {
			return err
		}
		if !validArtifactPath(resultPath) {
			return errInvalid(CodeSchemaInvalid, "result_path", "unsafe path "+resultPath)
		}
		dirfd, err := openSecureDir(spool)
		if err != nil {
			return err
		}
		defer unix.Close(dirfd)
		raw, err := readLeafFile(dirfd, resultPath, limits.EnvelopeBytes)
		if err != nil {
			return err
		}
		res, err := DecodeResult(adm, raw, peer, limits)
		if err != nil {
			return err
		}
		digest := Digest(raw)
		if final, ok := st.finalBy[ref.Identity.ExecutionID]; ok {
			if final.Acknowledgement.ResultDigest == digest {
				ack = *final.Acknowledgement
				return nil
			}
			qack := Acknowledgement{State: "quarantined", ExecutionID: ref.Identity.ExecutionID, TaskDigest: ref.TaskDigest, ResultDigest: digest}
			qerr := errInvalid(CodeResultConflict, "result_digest", "a different final result exists for the execution")
			if _, dup := st.quarantine[ref.Identity.ExecutionID+"\x00"+digest]; dup {
				ack = qack
				return qerr
			}
			cres, err := cloneResult(res)
			if err != nil {
				return err
			}
			candidate := qack
			if err := app(Record{Kind: "quarantined", Result: &cres, Acknowledgement: &candidate}); err != nil {
				return err
			}
			ack = qack
			return qerr
		}
		if err := validateArtifactsAt(dirfd, res, limits); err != nil {
			return err
		}
		cres, err := cloneResult(res)
		if err != nil {
			return err
		}
		candidate := Acknowledgement{State: "received", ExecutionID: ref.Identity.ExecutionID, TaskDigest: ref.TaskDigest, ResultDigest: digest}
		if err := app(Record{Kind: "received", Result: &cres, Acknowledgement: &candidate}); err != nil {
			return err
		}
		ack = candidate
		return nil
	})
	return ack, err
}

func (r *Registry) Receipt(ref Reference) (Result, Acknowledgement, error) {
	var res Result
	var ack Acknowledgement
	err := r.transact(func(st *ledgerState, _ func(Record) error) error {
		if _, err := r.currentAdmission(st, ref); err != nil {
			return err
		}
		final, ok := st.finalBy[ref.Identity.ExecutionID]
		if !ok {
			return errInvalid(CodeResultInvalid, "receipt", "the execution has no received result")
		}
		cres, err := cloneResult(*final.Result)
		if err != nil {
			return err
		}
		res = cres
		ack = *final.Acknowledgement
		return nil
	})
	return res, ack, err
}
