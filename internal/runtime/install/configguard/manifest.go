package configguard

const InstallManifestName = ".crw-install.json"

type FailureRecord struct {
	ExitCode float64
	Message  string
}
type FlagRecord struct {
	PriorEnabled, EnabledByCodexclaw, EnableFailed bool
	Failure                                        *FailureRecord
}
type TableKeyRecord struct {
	Table, Key     string
	PriorValue     *string
	AppliedValue   string
	SetByCodexclaw bool
}
type InstallManifest struct {
	Version                      int
	ActivatedAt, ConfigPath      string
	BackupPath, PostActivateHash *string
	Flags                        map[string]FlagRecord
	TableKeys                    map[string]TableKeyRecord
	flagOrder, tableOrder        []string
}

func parseInstallManifest(string) *InstallManifest   { return nil }
func manifestPath(home string) string                { return home + "/" + InstallManifestName }
func manifestBytes(*InstallManifest) ([]byte, error) { return []byte("{}\n"), nil }
