package interview

import "io"

// Red-first stub of the freeze command port (CRW-437): the types are final, the behaviour is not written yet.

// SessionReader is the session read of the command: the session's slug and its interview tracker.
type SessionReader func(cwd, sessionID string) (slug string, tracker *Tracker)

// FreezeCliArgs are the arguments of crw pabcd freeze.
type FreezeCliArgs struct {
	Cwd       string
	SessionID string
	DryRun    bool
	Help      bool
}

func ParseFreezeArgs(argv []string) (FreezeCliArgs, error) { return FreezeCliArgs{}, nil }

func ListPlanFiles(planDir string) ([]PlanFileHash, error) { return nil, nil }

func RunFreeze(args FreezeCliArgs, read SessionReader) (string, error) { return "", nil }

func FreezeCommand(argv []string, stdout, stderr io.Writer, read SessionReader) int { return 0 }
