package cli

// Reported by doctor, copied from cli.py OFFLINE_COMMANDS and HOST_REQUIRED_COMMANDS.
var offlineCommands = []string{
	"ack", "ack-proof", "admit-turn", "assignment-show", "claim", "criteria-register",
	"criteria-show", "doctor", "emit", "generation-bind", "generation-open", "register",
	"relationship-resume", "relationship-status", "revision-head", "settings-record", "settings-show",
	"show", "status", "store-challenge", "store-identity", "verdict", "linkage-attach",
	"linkage-bind", "linkage-counterpart", "linkage-directive", "linkage-completion", "linkage-down",
	"linkage-handover", "linkage-outstanding", "linkage-peer", "linkage-settle", "linkage-supervise",
	"linkage-up", "fault-target", "fault-observe", "fault-sweep", "fault-show", "fault-fix",
	"fault-reverify", "fault-resolve", "fault-next", "fault-claim", "fault-operation",
	"fault-reconcile", "fault-complete", "fault-fail", "fault-retry", "fault-prune", "fault-adopt",
	"fault-move", "fault-queue", "fault-update", "fault-cancel", "fault-stage", "fault-policy",
	"fault-limit", "fault-attention", "fault-relink", "fault-notifications",
	"fault-notification-raise", "fault-notification-reserve", "fault-notification-ack",
	"fault-notification-fail", "fault-notification-reconcile", "product-register", "product-bind",
	"product-show", "route-policy", "route-intake", "route-classify", "route-reconcile", "route-show",
	"route-digest", "route-projects", "completion-check", "service status", "service enable",
	"service disable", "service stop", "service declare", "dispositions-show", "managed-show",
	"managed-release", "reporting-show", "capacity-show", "limit-declare", "merge-turn-attest",
	"merge-turn-check", "merge-turn-land", "merge-turn-ready", "merge-turn-release",
	"merge-turn-request", "merge-turn-request-return", "merge-turn-resolve", "merge-turn-show",
	"merge-turn-unknown", "merge-turn-withdraw", "merge-turn-acknowledge", "merge-turn-restate-base",
	"region-followup", "region-followup-accept", "region-followup-settle", "region-propose",
	"region-reaffirm", "region-restate-revision", "region-settle", "region-show", "slot-release",
	"slot-reserve", "usage-observe", "supervisor-select", "supervisor-standing",
	"supervisor-report-recorded", "reporting-derive", "supervisor-stage", "supervisor-show",
	"packet-check", "merge-evidence", "intent-declare", "intent-attempt", "intent-bind",
	"intent-register", "intent-claim", "intent-disposition", "intent-resolve", "intent-show",
	"guard-evaluate",
}
var hostRequiredCommands = []string{
	"daemon", "deliver", "reconcile", "recover", "service run", "service start", "service restart",
	"verify-acks", "managed-start", "supervisor-send", "supervisor-read",
}
