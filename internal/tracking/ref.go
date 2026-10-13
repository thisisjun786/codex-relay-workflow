package tracking

// Ref names an issue or milestone as "<repo>#N": Repo is "owner/name" and Number is the issue or
// milestone number.
type Ref struct {
	Repo   string
	Number int
}

// String is the identifier "<repo>#N".
func (r Ref) String() string { return "" }

// ParseRef reads "<owner>/<name>#N".
func ParseRef(s string) (Ref, error) { return Ref{}, errNotImplemented }

// ParseBlockedBy returns the issues named on the "Blocked by:" lines of an issue body, in order and
// once each. A bare "#N" means issue N of repo, the repository of the issue the body belongs to.
func ParseBlockedBy(repo, body string) []Ref { return nil }

func checkHeader(header string) error { return errNotImplemented }
