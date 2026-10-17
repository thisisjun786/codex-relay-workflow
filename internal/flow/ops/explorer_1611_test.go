package ops

import (
	"context"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

func TestExplorer1611NormalizedFindingsConsumer(t *testing.T) {
	missing := `{"answer":"found it","evidence":["README.md"],"inferred":[],"unknown":[]}`
	missingEnd := `{"answer":"found it","evidence":["README.md"],"inferred":[],"unknown":[],"end_conditions":[]}`
	for name, tc := range map[string]struct{ imports, body, missing string }{
		"findings": {
			imports: "import { FINDINGS } from 'crw:lib/schemas'\nexport const meta = { name: 't' }\n",
			body:    `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS, label: 'exp' }); return r;`,
			missing: missing,
		},
		"findings_end": {
			imports: "import { FINDINGS_END } from 'crw:lib/schemas'\nexport const meta = { name: 't' }\n",
			body:    `const r = await agent.call('explore', { agentType: 'explorer', schema: FINDINGS_END, label: 'exp' }); return r;`,
			missing: missingEnd,
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := engine.NewFake()
			journal := engine.NewMemoryJournal()
			fake.Handler = func(engine.FakeRequest) engine.FakeReply {
				return engine.FakeReply{Text: tc.missing}
			}
			p, err := script.Load(script.Source{Name: "t.js", Text: []byte(tc.imports + tc.body)})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			res, err := engine.Run(ctx, p, engine.Config{Backend: fake, Policy: engine.FakePolicy(), Journal: journal, RunID: "run-test"})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Calls) != 1 || res.Calls[0].Status != engine.StatusCompleted {
				t.Fatalf("calls = %+v", res.Calls)
			}
			f, err := DecodeFindings(res.Calls[0].Output)
			if err != nil {
				t.Fatal(err)
			}
			if f.Verified == nil || len(f.Verified) != 0 {
				t.Fatalf("verified = %v", f.Verified)
			}
			if name == "findings_end" && (f.EndConditions == nil || len(f.EndConditions) != 0) {
				t.Fatalf("end_conditions = %v", f.EndConditions)
			}
			schemaName := "FINDINGS"
			if name == "findings_end" {
				schemaName = "FINDINGS_END"
			}
			if err := Validate(schemaName, []byte(tc.missing)); err == nil {
				t.Fatalf("the public %s schema still accepts the missing verified payload", schemaName)
			}
		})
	}
}
