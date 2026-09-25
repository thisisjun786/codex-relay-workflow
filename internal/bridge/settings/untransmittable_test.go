package settings

import "testing"

// Both branches of settings.py's "only {sorted(mapped) or 'no field'} is transmittable" suffix,
// as the Python SettingsContract raises them for the same policies.
func Test_an_untransmittable_field_names_what_this_type_can_carry(t *testing.T) {
	for _, tc := range []struct {
		sandbox string
		policy  map[string]any
		want    string
	}{
		{"workspace-write", map[string]any{"type": "workspaceWrite", "weird": true}, "setting_untransmittable: workspaceWrite weird cannot be carried by thread/start or thread/resume; only ['excludeSlashTmp', 'excludeTmpdirEnvVar', 'networkAccess', 'writableRoots'] is transmittable for this type"},
		{"read-only", map[string]any{"type": "readOnly", "networkAccess": true}, "setting_untransmittable: readOnly networkAccess cannot be carried by thread/start or thread/resume; only no field is transmittable for this type"},
	} {
		err := Contract{CWD: "/x", Sandbox: tc.sandbox, Model: "m", ReasoningEffort: "h", ExpectedPolicy: tc.policy}.Validate()
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v\nwant %s", tc.sandbox, err, tc.want)
		}
	}
}
