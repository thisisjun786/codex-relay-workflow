package agy

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// failureTexts map what agy says when it fails to a reason, lower case, tried in order against the envelope's error and stderr. They are agy 1.2.16's own
// words from R0, except the authentication ones, which are the wording of agy's headless documentation.
var failureTexts = []struct {
	text   string
	reason Reason
}{
	{"resource_exhausted", ReasonQuota},
	{"quota reached", ReasonQuota},
	{"authentication required", ReasonAuthentication},
	{"unauthenticated", ReasonAuthentication},
	{"invalid model selection", ReasonUnknownModel},
	{"blocked by content safety filters", ReasonContentFilter},
}

func (r Result) with(class Class, reason Reason, format string, args ...any) Result {
	r.Class, r.Reason, r.Detail = class, reason, fmt.Sprintf(format, args...)
	return r
}

// firstLine is the first line of s, cut to 240 characters.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if utf8.RuneCountInString(line) > 240 {
		line = string([]rune(line)[:240]) + "..."
	}
	return line
}

// classify decides the class of a finished call from what the process did, agy's log, the length of the prompt sent and whether a schema was asked for.
func classify(ex execution, logText string, promptBytes int, schema bool) Result {
	lg := parseLog(logText)
	res := Result{ExitCode: ex.exitCode, Elapsed: ex.elapsed, Limit: ex.limit, Model: lg.served}
	env, envErr := parseEnvelope(ex.stdout)
	if envErr == nil {
		res.Usage = env.Usage
	}
	stderr := string(ex.stderr)
	switch {
	case ex.startErr != nil:
		return res.with(ClassUnavailable, ReasonNotStarted, "agy did not start: %v", ex.startErr)
	case ex.timedOut:
		return res.with(ClassInvalid, ReasonTimeLimit, "agy was killed after the call time limit of %s", ex.limit)
	case ex.truncated:
		return res.with(ClassUnavailable, ReasonCrash, "agy wrote more output than the cap keeps")
	case ex.readErr != nil:
		return res.with(ClassUnavailable, ReasonCrash, "reading agy's output failed: %v", ex.readErr)
	case ex.exitCode < 0:
		return res.with(ClassUnavailable, ReasonCrash, "agy was killed by a signal: %s", firstLine(stderr))
	case ex.exitCode != 0:
		return res.failed(env, envErr, stderr, fmt.Sprintf("exit %d", ex.exitCode))
	case envErr != nil:
		return res.with(ClassUnavailable, ReasonCrash, "%v", envErr)
	case env.Status != "SUCCESS":
		return res.failed(env, envErr, stderr, "status "+env.Status)
	case lg.promptLength < 0:
		return res.with(ClassInvalid, ReasonPromptLength, "agy's log has no promptLength line; the prompt is %d bytes", promptBytes)
	case lg.promptLength != promptBytes:
		return res.with(ClassInvalid, ReasonPromptLength, "agy's log says promptLength=%d; the prompt is %d bytes", lg.promptLength, promptBytes)
	case strings.Contains(stderr, "print timeout after") || strings.Contains(logText, "print timeout after"):
		return res.with(ClassInvalid, ReasonPartialResponse, "agy returned partial output at its own print timeout")
	case len(env.DeniedActions) > 0:
		names := make([]string, len(env.DeniedActions))
		for i, a := range env.DeniedActions {
			names[i] = a.DisplayName
		}
		return res.with(ClassInvalid, ReasonDeniedActions, "agy refused %s", strings.Join(names, ", "))
	case strings.TrimSpace(env.Response) == "":
		return res.with(ClassInvalid, ReasonEmptyResponse, "the response is empty")
	case schema && !env.hasStructuredOutput():
		return res.with(ClassInvalid, ReasonNoStructured, "a schema was given and the envelope has no structured_output")
	}
	res.Class = ClassNormal
	if schema {
		res.StructuredOutput = env.StructuredOutput
	}
	return res
}

// failed classifies a call that failed: by what agy said, else as a crash. what says how the call ended (an exit code, a signal or a status).
func (r Result) failed(env envelope, envErr error, stderr, what string) Result {
	text := stderr
	if envErr == nil {
		text = env.Error + "\n" + stderr
	}
	lower := strings.ToLower(text)
	for _, f := range failureTexts {
		if strings.Contains(lower, f.text) {
			return r.with(ClassUnavailable, f.reason, "%s", firstLine(text))
		}
	}
	if line := firstLine(text); line != "" {
		what += ": " + line
	}
	return r.with(ClassUnavailable, ReasonCrash, "%s", what)
}
