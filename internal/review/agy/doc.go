// Package agy is the one place the independent code review calls agy (the Antigravity CLI): Run sends a prompt on stdin, checks that agy received
// all of it, decides whether the answer can be trusted and hands back the structured output with the class of the call. It makes no prompt, reads no
// repository and writes no artifact; the pipeline above it supplies the prompt text and the JSON Schema and maps the Result into the review model.
//
// # The call
//
//	agy --model <Config.Model> --output-format json [--json-schema <file>] --disable-slash-commands --log-file <file>
//
// The prompt goes to stdin, a pipe and not a terminal. The arguments never hold --print=- (agy 1.2.16 then reads "-" as the whole prompt and exits 0), -p,
// --print-timeout, --mode, --sandbox or --dangerously-skip-permissions; R0 measured --mode plan and --sandbox as no isolation. The model is
// Config.Model, DefaultModel (gemini-3.8-flash-high) when empty; changing the model is changing that one value.
//
// # Classes
//
// Every call ends in one Class with a Reason. A call that is not normal is never "no findings"; its StructuredOutput is empty.
//
//	normal       none                    a structured output (when a schema was given) and a non-empty response, nothing denied
//	invalid      denied_actions          the envelope lists actions agy refused (a refused tool call ends the turn with exit 0)
//	             empty_response          exit 0 and a blank response
//	             no_structured_output    a schema was given and the envelope has no structured_output
//	             print_timeout_partial   agy cut the turn at its own print timeout and returned what it had
//	             time_limit_exceeded     the call time limit passed and the process group was killed
//	             prompt_length_mismatch  agy's log has no promptLength line, or another length than the prompt sent
//	unavailable  quota                   RESOURCE_EXHAUSTED or "quota reached" (exit 3)
//	             authentication          an "authentication required" error (wording from agy's headless documentation; R0 saw no such failure)
//	             unknown_model           "invalid model selection" (exit 1)
//	             content_filter          "blocked by content safety filters" (exit 3)
//	             crash                   any other non-zero exit, a signal, output over the cap or not exactly one JSON envelope, a read error (a descendant of agy
//	                                     still held stdout or stderr after agy exited, so the output cannot be known to be whole), a status other than SUCCESS
//	             not_started             the agy binary was not found or did not start
//	             lock_wait_expired       the host-wide lock was not free within Config.LockWait; agy was not started
//
// The checks run in this order: not started, time limit, read error, output cap, signal, non-zero exit, output that is not exactly one JSON envelope (nothing
// before or after it but whitespace), status, prompt length, print timeout, denied actions, empty response, missing structured output. A non-zero exit is matched only against the envelope's error and stderr, never against the
// response text or agy's log (which says "not logged into Antigravity" on every call); a signal is a crash whatever stderr says. The prompt length comes first
// among the checks of a finished call because the answer to a mangled prompt (the "-" case) is typically empty or denied, and the length names the cause.
//
// # Prompt length
//
// agy logs "Print mode: starting (promptLength=N, ...)" in the file given with --log-file. N is the number of bytes of the prompt: in R0's recordings
// of 128 prompts, the 21 that held non-ASCII text all matched the byte length and none the rune count or the UTF-16 length. The runner compares N with
// len(Request.Prompt), so the prompt is sent exactly as given, with nothing added.
//
// # Where things live
//
// Each call makes a directory under Config.WorkRoot: work/ (empty, the working directory of agy), schema.json and agy.log (agy's per-call log, read
// for the prompt length and the served model, never returned). The directory is removed after every outcome, before the lock is released.
//
// # Limits and isolation
//
// The call time limit is Config.TimeLimitFloor for a prompt up to 100 KiB and grows in proportion beyond that, up to Config.TimeLimitCeiling. agy runs in a
// process group of its own, led by a sentinel shell that nobody reaps until the last signal has been sent, so the group id cannot be taken by another process
// meanwhile; only that group is signalled: when the limit passes, and again once agy has exited, after which the call waits up to two seconds for the group
// to be empty. A group that is not empty then makes the call unavailable (crash), so a descendant neither outlives the call or its lock unnoticed nor lets
// its answer count. At most one call runs on the host: Run holds an exclusive flock on Config.LockPath for the whole
// call and waits for it for at most Config.LockWait; the file is never removed, because replacing it would let two holders in. agy gets an allowlisted
// environment, by exact name (PATH, HOME, USER, LOGNAME, TZ, TERM, TMPDIR, the XDG_ directories, LANG, LANGUAGE, the LC_ categories, SSL_CERT_*, the proxy
// variables) and the caller cannot add to it, so GH_TOKEN, GITHUB_TOKEN, SSH_AUTH_SOCK and every other credential stay behind while agy still finds its own
// login under HOME and XDG_CONFIG_HOME.
//
// Run returns an error only for a caller that cancelled, an empty prompt or a failure of the host (lock file, working directory); every outcome of agy
// itself, including a lock that stayed busy, is a Result. Text agy wrote is data: nothing here executes, evaluates or follows it.
package agy
