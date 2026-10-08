package shellir

import "strings"

// unwrapped is what a wrapper hands on: the commands it runs and the
// environment assignments it applies to them.
type unwrapped struct {
	inner   [][]Word
	assigns []Assign
}

// unwrapCommand applies the option grammar of one wrapper. An option the
// grammar does not list makes the program position unproven.
func unwrapCommand(name string, args []Word) (unwrapped, error) {
	var u unwrapped
	switch name {
	case "env":
		return unwrapEnv(args)
	case "find":
		return unwrapFind(args)
	case "busybox":
		if len(args) == 0 {
			return u, nil
		}
		if v, err := knownValue(args[0], "busybox applet"); err != nil {
			return u, err
		} else if strings.HasPrefix(v, "-") {
			return u, unreadablef("busybox option %s is not modelled", v)
		}
		u.inner = [][]Word{args}
		return u, nil
	}
	idx, err := wrapperOptions(name, args)
	if err != nil {
		return u, err
	}
	if name == "command" {
		for _, a := range args[:idx] {
			if strings.ContainsAny(a.Value, "vV") {
				return u, nil
			}
		}
	}
	if name == "timeout" {
		if idx >= len(args) {
			return u, unreadablef("timeout without a duration")
		}
		idx++
	}
	if idx < len(args) {
		rest := args[idx:]
		if name == "parallel" {
			rest = parallelCommand(rest)
		}
		if len(rest) > 0 {
			u.inner = [][]Word{rest}
		}
	}
	return u, nil
}

// wrapperOptions returns the index of the first word after the wrapper's own
// options. Each wrapper lists the letters it takes, the letters that take a value,
// and the letters that may carry an attached value.
func wrapperOptions(name string, args []Word) (int, error) {
	switch name {
	case "command":
		return skipOptions(name, args, "pvV", "", "")
	case "builtin", "nohup":
		return skipOptions(name, args, "", "", "")
	case "exec":
		return skipOptions(name, args, "cl", "a", "")
	case "nice":
		return skipNice(args)
	case "ionice":
		return skipOptions(name, args, "t", "cn", "")
	case "timeout":
		return skipOptions(name, args, "pv", "ks", "")
	case "time":
		return skipOptions(name, args, "paqvi", "f", "")
	case "stdbuf":
		return skipOptions(name, args, "", "ioe", "")
	case "setsid":
		return skipOptions(name, args, "cfw", "", "")
	case "sudo":
		return skipOptions(name, args, "EHnSkb", "ug", "")
	case "doas":
		return skipOptions(name, args, "n", "u", "")
	case "xargs":
		return skipOptions(name, args, "0rtxpe", "ILnPdEsa", "il")
	case "parallel":
		return skipOptions(name, args, "0kqv", "jnaX", "")
	}
	return 0, unreadablef("wrapper %s has no option grammar", name)
}

func skipOptions(name string, args []Word, flags, valued, optional string) (int, error) {
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], name+" option")
		if err != nil {
			return 0, err
		}
		if v == "--" {
			return i + 1, nil
		}
		if len(v) < 2 || v[0] != '-' {
			return i, nil
		}
		if strings.HasPrefix(v, "--") {
			return 0, unreadablef("%s option %s is not modelled", name, v)
		}
		i++
		for k := 1; k < len(v); k++ {
			c := v[k]
			last := k == len(v)-1
			switch {
			case strings.IndexByte(valued, c) >= 0:
				if last {
					if i >= len(args) {
						return 0, unreadablef("%s -%c without a value", name, c)
					}
					i++
				}
				k = len(v)
			case strings.IndexByte(optional, c) >= 0:
				k = len(v)
			case strings.IndexByte(flags, c) >= 0:
			default:
				return 0, unreadablef("%s option -%c is not modelled", name, c)
			}
		}
	}
	return i, nil
}

func skipNice(args []Word) (int, error) {
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], "nice option")
		if err != nil {
			return 0, err
		}
		switch {
		case v == "--":
			return i + 1, nil
		case v == "-n" && i+1 < len(args):
			i += 2
		case strings.HasPrefix(v, "--adjustment="):
			i++
		case isDashDigits(v):
			i++
		default:
			if strings.HasPrefix(v, "-") && len(v) > 1 {
				return 0, unreadablef("nice option %s is not modelled", v)
			}
			return i, nil
		}
	}
	return i, nil
}

func isDashDigits(v string) bool {
	if len(v) < 2 || v[0] != '-' {
		return false
	}
	for _, c := range v[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parallelCommand keeps the command template and drops the ':::' argument list.
func parallelCommand(args []Word) []Word {
	for i, a := range args {
		if a.Value == ":::" || a.Value == "::::" {
			return args[:i]
		}
	}
	return args
}

func unwrapEnv(args []Word) (unwrapped, error) {
	var u unwrapped
	for len(args) > 0 {
		v, err := knownValue(args[0], "env option")
		if err != nil {
			return u, err
		}
		if v == "-" || v == "-i" || v == "--ignore-environment" || v == "-0" || v == "--null" {
			args = args[1:]
			continue
		}
		if v == "-u" || v == "--unset" {
			if len(args) < 2 {
				return u, unreadablef("env -u without a name")
			}
			args = args[2:]
			continue
		}
		if strings.HasPrefix(v, "--unset=") {
			args = args[1:]
			continue
		}
		if v == "-S" || v == "--split-string" || strings.HasPrefix(v, "--split-string=") {
			var s string
			if strings.HasPrefix(v, "--split-string=") {
				s = strings.TrimPrefix(v, "--split-string=")
				args = args[1:]
			} else {
				if len(args) < 2 {
					return u, unreadablef("env -S without a string")
				}
				if s, err = knownValue(args[1], "env -S string"); err != nil {
					return u, err
				}
				args = args[2:]
			}
			split, err := splitEnvString(s)
			if err != nil {
				return u, err
			}
			args = append(split, args...)
			continue
		}
		if v == "--" {
			args = args[1:]
			break
		}
		if strings.HasPrefix(v, "-") && len(v) > 1 {
			return u, unreadablef("env option %s is not modelled", v)
		}
		if asgName, val, ok := splitAssignment(v); ok {
			u.assigns = append(u.assigns, Assign{Name: asgName, Value: Word{Known: true, Value: val}})
			args = args[1:]
			continue
		}
		break
	}
	if len(args) > 0 {
		u.inner = [][]Word{args}
	}
	return u, nil
}

// splitEnvString splits the text of env -S. Quoting, expansion and escapes
// change the words, so any of them makes the split unproven.
func splitEnvString(s string) ([]Word, error) {
	if strings.ContainsAny(s, "$\\'\"#\x60") {
		return nil, unreadablef("env -S text with quoting, expansion or comment is not modelled")
	}
	fields := strings.Fields(s)
	out := make([]Word, 0, len(fields))
	for _, f := range fields {
		out = append(out, Word{Known: true, Value: f})
	}
	return out, nil
}

func splitAssignment(v string) (string, string, bool) {
	i := strings.IndexByte(v, '=')
	if i <= 0 || !validName(v[:i]) {
		return "", "", false
	}
	return v[:i], v[i+1:], true
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// unwrapFind returns each program that find -exec, -execdir, -ok or -okdir runs.
func unwrapFind(args []Word) (unwrapped, error) {
	var u unwrapped
	for _, a := range args {
		if !a.Known {
			return u, unreadablef("find argument is not known (%s)", a.Reason)
		}
	}
	for i := 0; i < len(args); i++ {
		switch args[i].Value {
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j].Value != ";" && args[j].Value != "+" {
				j++
			}
			if j >= len(args) {
				return u, unreadablef("find %s without a terminator", args[i].Value)
			}
			if j == i+1 {
				return u, unreadablef("find %s without a program", args[i].Value)
			}
			u.inner = append(u.inner, args[i+1:j])
			i = j
		}
	}
	return u, nil
}
