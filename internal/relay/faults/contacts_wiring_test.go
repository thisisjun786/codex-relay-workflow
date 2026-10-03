package faults_test

// The supervisor package installs the contact reading that the fault commands and the deliverer ask
// for (faults.SetContacts) when it is linked, and the faults tests run those commands. Linking it into
// this test binary is how the reading they exercise gets there; no test here refers to it.
import _ "github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
