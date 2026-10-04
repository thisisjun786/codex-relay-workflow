package role

type FailureDecision struct {
	Code   *string `json:"code"`
	Action string  `json:"action"`
}

func decodeDispatchFailure(any) FailureDecision { return FailureDecision{Action: "unknown"} }
