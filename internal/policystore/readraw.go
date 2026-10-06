package policystore

// ReadRaw reads the policy file's bytes through the same non-blocking, descriptor-judged reader the
// reading half uses, so a path that names a named pipe or any other non-regular file is refused
// instead of blocking the request.
func ReadRaw(path string) ([]byte, error) {
	return readRegular(path)
}
