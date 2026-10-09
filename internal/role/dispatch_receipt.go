package role

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
}

// DispatchIssuance is the spawn hook's record of the native call the attempt was issued to.
type DispatchIssuance struct {
	Recorded  bool    `json:"recorded"`
	ToolUseID *string `json:"toolUseId"`
	// Reconciliation is the caller's evidence for a child recorded without issuance.
	Reconciliation *string `json:"reconciliation,omitempty"`
}

// DispatchChild is the child the host witnesses: its id, its parent and the host surface that answered.
type DispatchChild struct {
	AgentID string `json:"agentId"`
	Parent  string `json:"parentThreadId"`
	Witness string `json:"witness"`
}

// DispatchHostSettings are the model and effort the host's thread row shows for the child; Source is
// "native-thread-database", or "unobservable" when the host shows neither.
type DispatchHostSettings struct {
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
	Source string  `json:"source"`
}
