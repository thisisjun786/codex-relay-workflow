package shellir

import "strings"

// optVal is one option of a wrapper's grammar with the value it carried (the attached rest of its word or the next word).
type optVal struct {
	c byte
	v string
}

// scanOptions reads a wrapper's options as its real grammar gives them: flags, valued letters (a value attached or the next
// word) and optional-value letters (attached or none). It returns the index of the first word after the options and the
// option values seen. An option the grammar does not list makes the program position unproven.
func scanOptions(name string, args []Word, flags, valued, optional string) (int, []optVal, error) {
	i := 0
	var got []optVal
	for i < len(args) {
		v, err := knownValue(args[i], name+" option")
		if err != nil {
			return 0, nil, err
		}
		if v == "--" {
			return i + 1, got, nil
		}
		if len(v) < 2 || v[0] != '-' {
			return i, got, nil
		}
		if strings.HasPrefix(v, "--") {
			return 0, nil, unreadablef("%s option %s is not modelled", name, v)
		}
		i++
		for k := 1; k < len(v); k++ {
			c := v[k]
			last := k == len(v)-1
			switch {
			case strings.IndexByte(valued, c) >= 0:
				if last {
					if i >= len(args) {
						return 0, nil, unreadablef("%s -%c without a value", name, c)
					}
					val, err := knownValue(args[i], name+" option value")
					if err != nil {
						return 0, nil, err
					}
					got = append(got, optVal{c, val})
					i++
				} else {
					got = append(got, optVal{c, v[k+1:]})
				}
				k = len(v)
			case strings.IndexByte(optional, c) >= 0:
				got = append(got, optVal{c, v[k+1:]})
				k = len(v)
			case strings.IndexByte(flags, c) >= 0:
				got = append(got, optVal{c, ""})
			default:
				return 0, nil, unreadablef("%s option -%c is not modelled", name, c)
			}
		}
	}
	return i, got, nil
}

func optSeen(got []optVal, c byte) (string, bool) {
	for _, o := range got {
		if o.c == c {
			return o.v, true
		}
	}
	return "", false
}

// FileRecordName names the synthetic record of a wrapper's own files (script's transcript and log files, strace -o). Its
// operands are all files, none of them an option, so the hook reader takes every word verbatim.
const FileRecordName = "wrapper-file"

func fileRecord(files []string) (string, []Word) {
	words := make([]Word, 0, len(files))
	for _, f := range files {
		if f != "" {
			words = append(words, Word{Known: true, Value: f})
		}
	}
	return FileRecordName, words
}

// unwrapRunner handles the wrappers that run a program the text shows in another form: a shell string (watch, flock -c,
// script -c, entr -s), an argv with a lock or root operand (flock, chroot), a traced program with its own output file
// (strace, ltrace), an argv whose operands arrive at run time (entr), and programs read from standard input (at, batch).
// handled is false for every other name.
func unwrapRunner(name string, args []Word) (u unwrapped, handled bool, err error) {
	switch name {
	case "watch":
		// watch joins its command into a shell string; -x runs the argv directly.
		idx, got, err := scanOptions("watch", args, "bcegptqx", "n", "d")
		if err != nil {
			return u, true, err
		}
		rest := args[idx:]
		if len(rest) == 0 {
			return u, true, nil
		}
		if _, x := optSeen(got, 'x'); x {
			u.inner = [][]Word{rest}
			return u, true, nil
		}
		text, err := joinKnown(rest, "watch command")
		if err != nil {
			return u, true, err
		}
		u.shell, u.shellCarrier, u.isShell = text, "watch", true
		return u, true, nil
	case "flock":
		u, err := unwrapFlock(args)
		return u, true, err
	case "chroot":
		u, err := unwrapChroot(args)
		return u, true, err
	case "script":
		// script -c runs $SHELL -c COMMAND; without -c it starts an interactive shell, which the text does not show.
		idx, got, err := scanOptions("script", args, "qefa", "cBIOTE", "t")
		if err != nil {
			return u, true, err
		}
		text, ok := optSeen(got, 'c')
		if !ok {
			return u, true, unreadablef("script without -c starts an interactive shell")
		}
		var files []string
		logged := false // -I, -O or -B name an output log, which takes the place of the default transcript
		for _, o := range got {
			if strings.IndexByte("tTBIO", o.c) >= 0 {
				files = append(files, o.v)
			}
			logged = logged || strings.IndexByte("BIO", o.c) >= 0
		}
		if rest := args[idx:]; len(rest) > 0 {
			f, err := knownValue(rest[0], "script file")
			if err != nil {
				return u, true, err
			}
			files = append(files, f)
		} else if !logged {
			// Without a file operand and without -I, -O or -B script writes its transcript to typescript in the current
			// directory (util-linux 2.41.3 on the host: -T alone and -a still write typescript; -O, -I or -B alone do not).
			files = append(files, "typescript")
		}
		u.shell, u.shellCarrier, u.isShell = text, "script -c", true
		u.recordName, u.record = fileRecord(files)
		return u, true, nil
	case "strace", "ltrace":
		valued, flags := "oesEuabIOPSXp", "cCdDfFiqrtTvxyn"
		if name == "ltrace" {
			valued, flags = "aAeFlnopsuwx", "bcCdDfiLrStTz"
		}
		idx, got, err := scanOptions(name, args, flags, valued, "")
		if err != nil {
			return u, true, err
		}
		rest := args[idx:]
		if len(rest) == 0 {
			if _, p := optSeen(got, 'p'); p {
				return u, true, nil
			}
			return u, true, unreadablef("%s without a program", name)
		}
		var files []string
		for _, o := range got {
			if o.c == 'o' {
				files = append(files, o.v)
			}
		}
		u.recordName, u.record = fileRecord(files)
		u.inner = [][]Word{rest}
		return u, true, nil
	case "entr":
		// entr reads its file list from standard input and appends the names to the utility at run time; -s runs a shell string.
		idx, got, err := scanOptions("entr", args, "acdnprxzs", "", "")
		if err != nil {
			return u, true, err
		}
		rest := args[idx:]
		if len(rest) == 0 {
			return u, true, unreadablef("entr without a utility")
		}
		if _, s := optSeen(got, 's'); s {
			text, err := knownValue(rest[0], "entr -s utility")
			if err != nil {
				return u, true, err
			}
			u.shell, u.shellCarrier, u.isShell = text, "entr -s", true
			return u, true, nil
		}
		u.inner = [][]Word{rest}
		return u, true, nil
	case "at", "batch":
		return u, true, unreadablef("%s reads the program from standard input or a file", name)
	}
	return u, false, nil
}

// unwrapFlock reads flock: options (-c takes a shell string, attached or the next word), the lock file or descriptor, and
// then either -c or the program. Options are read again after the lock file, as getopt permutes them.
func unwrapFlock(args []Word) (unwrapped, error) {
	var u unwrapped
	cmd, hasCmd := "", false
	readOpts := func(i int) (int, error) {
		for i < len(args) {
			v, err := knownValue(args[i], "flock option")
			if err != nil {
				return 0, err
			}
			if v == "--" {
				return i + 1, nil
			}
			if len(v) < 2 || v[0] != '-' {
				return i, nil
			}
			i++
			for k := 1; k < len(v); k++ {
				c := v[k]
				last := k == len(v)-1
				switch {
				case c == 'c' || c == 'w' || c == 'E':
					var val string
					if last {
						if i >= len(args) {
							return 0, unreadablef("flock -%c without a value", c)
						}
						val, err = knownValue(args[i], "flock option value")
						if err != nil {
							return 0, err
						}
						i++
					} else {
						val = v[k+1:]
					}
					if c == 'c' {
						cmd, hasCmd = val, true
					}
					k = len(v)
				case strings.IndexByte("sxeunoF", c) >= 0:
				default:
					return 0, unreadablef("flock option -%c is not modelled", c)
				}
			}
		}
		return i, nil
	}
	i, err := readOpts(0)
	if err != nil {
		return u, err
	}
	if i >= len(args) {
		return u, unreadablef("flock without a lock file")
	}
	i++ // the lock file or descriptor
	i, err = readOpts(i)
	if err != nil {
		return u, err
	}
	if hasCmd {
		if i < len(args) {
			return u, unreadablef("flock arguments after -c")
		}
		u.shell, u.shellCarrier, u.isShell = cmd, "flock -c", true
		return u, nil
	}
	if i < len(args) {
		u.inner = [][]Word{args[i:]}
	}
	return u, nil
}

// unwrapChroot reads chroot: long options only (--skip-chdir, --userspec=, --groups=, --help, --version), the new root, then
// the program. Without a program chroot starts a shell the text does not show.
func unwrapChroot(args []Word) (unwrapped, error) {
	var u unwrapped
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], "chroot option")
		if err != nil {
			return u, err
		}
		if !strings.HasPrefix(v, "--") {
			break
		}
		switch {
		case v == "--skip-chdir", v == "--help", v == "--version",
			strings.HasPrefix(v, "--userspec="), strings.HasPrefix(v, "--groups="):
		default:
			return u, unreadablef("chroot option %s is not modelled", v)
		}
		i++
	}
	if i < len(args) {
		if v, err := knownValue(args[i], "chroot option"); err == nil && v == "--" {
			i++
		}
	}
	if i >= len(args) {
		return u, unreadablef("chroot without a program starts a shell")
	}
	i++ // the new root
	if i >= len(args) {
		return u, unreadablef("chroot without a program starts a shell")
	}
	u.inner = [][]Word{args[i:]}
	return u, nil
}
