package shellir

import "testing"

// TestOpaqueInterpreterOptionGrammar: the program position of php, lua, Rscript, tclsh, osascript and groovy is told from an
// option value by each interpreter's own option grammar: the value of -d is no script file, a code option with its value
// attached (-r'code', -e'code', --eval=code) is code, and an option outside the model is unreadable (CRW-1028 verifier finding 3).
func TestOpaqueInterpreterOptionGrammar(t *testing.T) {
	for _, cmd := range []string{
		"php -d display_errors=1 <<'EOF'\n<?php system(\"rm -rf ../repo\");\nEOF",
		"php -c php.ini <<'EOF'\n<?php echo 1;\nEOF",
		"php -n -q <<'EOF'\n<?php echo 1;\nEOF",
		`php -r'system("rm -rf ../repo");' ignored`,
		`php -r 'system("rm -rf ../repo");' ignored`,
		`php -rsystem ignored`,
		`php -dx=1 <<< '<?php echo 1;'`,
		`lua -e'print(1)' ignored.lua`,
		`lua -e 'print(1)' ignored.lua`,
		`lua -l mod ignored.lua`,
		`lua -i ignored.lua`,
		`Rscript --expression='print(1)' ignored.R`,
		`Rscript --expression=print(1) ignored.R`,
		`Rscript -e'print(1)' ignored.R`,
		`Rscript --no-such-option ignored.R`,
		"tclsh -encoding utf-8 <<'EOF'\nputs ok\nEOF",
		`osascript -e'beep' ignored.scpt`,
		`osascript -l JavaScript <<< 'x'`,
		`osascript -i ignored.scpt`,
		`groovy -e'println 1' ignored.groovy`,
		`groovy -e 'println 1' ignored.groovy`,
		`groovy --no-such-option ignored.groovy`,
		`php --no-such-option ignored.php`,
		`php -f /dev/stdin`,
		`php -f - ignored`,
		// the script operand after -- is judged as any other (verifier round 2)
		"php -- /dev/stdin <<'EOF'\n<?php echo 1;\nEOF",
		`php -- -`,
		`php -n -- /dev/fd/0`,
		`php -d x=1 -- /proc/self/fd/0`,
		`lua -- /dev/stdin`,
	} {
		analyzeUnreadable(t, cmd, true)
	}
	for _, cmd := range []string{
		"php -d display_errors=1 script.php",
		"php -dx=1 script.php arg",
		"php -n -q script.php",
		"php -c php.ini -f script.php",
		"php -f script.php",
		"php script.php -r",
		"lua -E script.lua arg",
		"lua script.lua -e",
		"Rscript --vanilla script.R",
		"Rscript --no-save --no-restore script.R arg",
		"tclsh -encoding utf-8 script.tcl",
		"osascript -l JavaScript script.scpt",
		"groovy script.groovy",
		"php -- script.php",
		"php -n -- script.php arg",
		"lua -- script.lua",
		"php --version",
		"lua -v",
	} {
		analyzeUnreadable(t, cmd, false)
	}
}
