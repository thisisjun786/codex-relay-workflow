package role

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
)

// The records the checked boundary adds to an attempt (receipt and its parts, termination, cleanup) keep the members they do
// not own. A stored record is read with its members as written (raw) and written back as those members with the ones the
// boundary owns set over them, as the attempt itself is, so another tool's members, and the candidate's extra members the
// receipt copies, survive every later write of the record.

// dispatchRawDecode decodes data into plain, a struct type without the methods of its record, and returns the members as read.
func dispatchRawDecode(data []byte, plain any) (object, error) {
	if string(data) == "null" {
		return nil, nil // as encoding/json leaves a struct for null
	}
	o, err := parseObject(data)
	if err != nil {
		return nil, err
	}
	return o, json.Unmarshal(data, plain)
}

// dispatchRawEncode writes plain, the record's struct type without its methods, over raw (nil: the record was not read from a
// file and plain is written as it is). The members plain names are set to what it holds, or removed when it omits them; every
// other member of raw stays where it was.
func dispatchRawEncode(raw object, plain any) ([]byte, error) {
	own, err := Stringify(plain, "")
	if err != nil || raw == nil {
		return own, err
	}
	written, err := parseObject(own)
	if err != nil {
		return nil, err
	}
	o := slices.Clone(raw)
	t := reflect.TypeOf(plain)
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if !t.Field(i).IsExported() || name == "" || name == "-" {
			continue
		}
		if value, ok := written.raw(name); ok {
			o.set(name, value)
		} else {
			o.remove(name)
		}
	}
	return o.MarshalJSON()
}

// DispatchReceipt is the one record of what a managed attempt asked for and what the host shows it got, written by the checked
// boundary when a created report is accepted. Each part names its source: the candidate the ledger chose, the issuance the
// spawn hook recorded, the child the host witnesses, how the child was tied to this attempt, the model the caller claims and
// the settings the host itself shows. Issuance is not proof of a provider or model.
type DispatchReceipt struct {
	Candidate DispatchCandidate `json:"candidate"`
	Issuance  DispatchIssuance  `json:"issuance"`
	Child     DispatchChild     `json:"child"`
	// Correlation is how the child is tied to this attempt: "spawn-result" (the host's own result of the issued native call, the
	// completed spawn item of its tool use id in the parent's rollout, names this child), "unverified" (issued, but the host
	// shows no result of the issued call to compare with) or "unissued" (recorded through the explicit reconciliation path
	// without an issuance). The dispatch marker in the child's first message only refuses a child; it never ties one. Only
	// "spawn-result" satisfies an independent review.
	Correlation string `json:"correlation"`
	// ObservedModel is the caller's observedModel, a claim and never evidence.
	ObservedModel *string              `json:"observedModel"`
	Host          DispatchHostSettings `json:"host"`
	raw           object
}

func (r *DispatchReceipt) UnmarshalJSON(data []byte) (err error) {
	type plain DispatchReceipt
	r.raw, err = dispatchRawDecode(data, (*plain)(r))
	return err
}

func (r DispatchReceipt) MarshalJSON() ([]byte, error) {
	type plain DispatchReceipt
	return dispatchRawEncode(r.raw, plain(r))
}

// DispatchIssuance is the spawn hook's record of the native call the attempt was issued to.
type DispatchIssuance struct {
	Recorded  bool    `json:"recorded"`
	ToolUseID *string `json:"toolUseId"`
	// Reconciliation is the caller's evidence for a child recorded without issuance.
	Reconciliation *string `json:"reconciliation,omitempty"`
	raw            object
}

func (i *DispatchIssuance) UnmarshalJSON(data []byte) (err error) {
	type plain DispatchIssuance
	i.raw, err = dispatchRawDecode(data, (*plain)(i))
	return err
}

func (i DispatchIssuance) MarshalJSON() ([]byte, error) {
	type plain DispatchIssuance
	return dispatchRawEncode(i.raw, plain(i))
}

// DispatchChild is the child the host witnesses: its id, its parent and the host surface that answered.
type DispatchChild struct {
	AgentID string `json:"agentId"`
	Parent  string `json:"parentThreadId"`
	Witness string `json:"witness"`
	raw     object
}

func (c *DispatchChild) UnmarshalJSON(data []byte) (err error) {
	type plain DispatchChild
	c.raw, err = dispatchRawDecode(data, (*plain)(c))
	return err
}

func (c DispatchChild) MarshalJSON() ([]byte, error) {
	type plain DispatchChild
	return dispatchRawEncode(c.raw, plain(c))
}

// DispatchHostSettings are the model and effort the host's thread row shows for the child; Source is
// "native-thread-database", or "unobservable" when the host shows neither.
type DispatchHostSettings struct {
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
	Source string  `json:"source"`
	raw    object
}

func (h *DispatchHostSettings) UnmarshalJSON(data []byte) (err error) {
	type plain DispatchHostSettings
	h.raw, err = dispatchRawDecode(data, (*plain)(h))
	return err
}

func (h DispatchHostSettings) MarshalJSON() ([]byte, error) {
	type plain DispatchHostSettings
	return dispatchRawEncode(h.raw, plain(h))
}
