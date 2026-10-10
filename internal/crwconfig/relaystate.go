package crwconfig

// ConfiguredRelayState is the durable relay state root the configuration file names (paths.relay_state),
// or "" when the file names none. It is the one reading every consumer of the relay's state selection
// shares, so the management tool, a relay started directly, the registered services and the install
// commands all land on the same bundle for one configuration and one socket. The root is the directory
// whose children are the relay's per-scope store directories; the caller joins the scope.
//
// A root that fails a structural check, and a paths key that points a part of the state apart, is
// refused with the check's words, so nothing opens a store against a split. A host that cannot name
// its configuration location at all has no configured root.
func ConfiguredRelayState(getenv func(string) string) (string, error) {
	file, err := loadOptional(getenv, "")
	if err != nil || file == nil {
		return "", err
	}
	root := file.roots[RootRelayState]
	if root.Source != SourceConfig {
		return "", nil
	}
	if err := file.Refusal(getenv, RootRelayState); err != nil {
		return "", err
	}
	return root.Path, nil
}
