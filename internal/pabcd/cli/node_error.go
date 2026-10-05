package cli

// nodeErrorMessage is the oracle's Error.message for err. The Node runtime spells a filesystem
// failure whose errno planErrno names as "<NAME>: <description>, <op> '<path>'", with the path left
// out for write and close; anything else keeps err.Error(). planFailure and the divergence candidate
// add branch share it (divergence-cli.ts:153 prints err.message verbatim).
//
// TODO(CRW-594): the conversion itself lands with the port; this stub keeps the red-first run
// compiling and answers exactly what dev prints today.
func nodeErrorMessage(err error) string {
	return err.Error()
}
