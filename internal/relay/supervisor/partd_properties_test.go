package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func directivePropertyWorld(t *testing.T) (*registry.Registry, *store.Store, string) {
	ctx := context.Background()
	s, err := store.Open(ctx, t.TempDir()+"/relay.sqlite3", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &registry.Registry{Store: s, Now: func() string { return "2026-01-01T00:00:00.000000+00:00" }, Policy: registry.ResolveRolePolicy(map[string]string{})}
	o, err := r.RegisterSupervision(ctx, "INI", "PRJ", registry.Endpoint{TaskID: "sup", HostID: "host"}, registry.Endpoint{TaskID: "parent", HostID: "host"}, "execution")
	if err != nil {
		t.Fatal(err)
	}
	var link string
	for _, f := range o {
		if f.Key == "linkId" {
			link = f.Value.(string)
		}
	}
	return r, s, link
}
func dref(t *testing.T, purpose, link, digest, correlation string) sql.NullString {
	t.Helper()
	s, err := registry.DirectiveReference(purpose, link, digest, correlation, correlation != "")
	if err != nil {
		t.Fatal(err)
	}
	return sql.NullString{String: s, Valid: true}
}
func did(o contract.OrderedObject) string {
	for _, f := range o {
		if f.Key == "directiveId" {
			return f.Value.(string)
		}
	}
	return ""
}
func refused(t *testing.T, err error) (string, string) {
	t.Helper()
	var e *store.RefusedError
	if !errors.As(err, &e) {
		t.Fatalf("not refusal: %v", err)
	}
	return e.Reason, e.Detail
}

func Test24_DIR_1_DirectivePlacesStandTogether(t *testing.T) {
	t.Parallel()
	r, _, l := directivePropertyWorld(t)
	ctx := context.Background()
	for _, x := range []struct{ d, p, c string }{{"a", "project_assignment", ""}, {"b", "relayed_decision", "m"}, {"c", "scope_correction", ""}} {
		if _, err := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, x.d, dref(t, x.p, l, x.d, x.c)); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := r.ContestedDirectives(ctx, "project", "PRJ")
	if len(v) != 0 {
		t.Fatal(v)
	}
}
func Test24_DIR_2_OneLiveAssignment(t *testing.T) {
	t.Parallel()
	r, _, l := directivePropertyWorld(t)
	ctx := context.Background()
	a, _ := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "a", dref(t, "project_assignment", l, "a", ""))
	_, err := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "b", dref(t, "project_assignment", l, "b", ""))
	reason, detail := refused(t, err)
	if reason != "link_conflict" || !strings.Contains(detail, did(a)) || !strings.Contains(detail, "linkage-settle") {
		t.Fatal(reason, detail)
	}
}
func Test24_DIR_3_OneAnswerPerMessage(t *testing.T) {
	t.Parallel()
	r, _, l := directivePropertyWorld(t)
	ctx := context.Background()
	a, _ := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "yes", dref(t, "relayed_decision", l, "yes", "m"))
	_, err := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "no", dref(t, "relayed_decision", l, "no", "m"))
	_, detail := refused(t, err)
	if !strings.Contains(detail, did(a)) {
		t.Fatal(detail)
	}
}
func Test24_DIR_4_OneScopeCorrection(t *testing.T) {
	t.Parallel()
	r, _, l := directivePropertyWorld(t)
	ctx := context.Background()
	_, _ = r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "a", dref(t, "scope_correction", l, "a", "msg-blocked-7"))
	_, err := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "b", dref(t, "scope_correction", l, "b", ""))
	_, detail := refused(t, err)
	if !strings.Contains(detail, "msg-blocked-7") {
		t.Fatal(detail)
	}
}
func Test24_DIR_5_PurposelessContest(t *testing.T) {
	t.Parallel()
	r, _, l := directivePropertyWorld(t)
	ctx := context.Background()
	_, _ = r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "a", sql.NullString{})
	_, _ = r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "b", sql.NullString{})
	v, _ := r.ContestedDirectives(ctx, "project", "PRJ")
	if len(v) != 2 {
		t.Fatal(v)
	}
}
func Test24_DIR_6_PlaceSurvivesHandover(t *testing.T) {
	t.Parallel()
	r, _, l := directivePropertyWorld(t)
	ctx := context.Background()
	a, _ := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "a", dref(t, "project_assignment", l, "a", ""))
	_, err := r.Handover(ctx, "supervisor", "INI", "sup", registry.Endpoint{TaskID: "next", HostID: "host"}, nil, "move", "actor")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.RecordDirective(ctx, "project", "PRJ", "next", "INI", l, "a", dref(t, "project_assignment", l, "a", ""))
	_, detail := refused(t, err)
	if !strings.Contains(detail, did(a)) || !strings.Contains(detail, "link revision 1") {
		t.Fatal(detail)
	}
}
func Test24_DIR_7_SameDigestOldWriterNoContest(t *testing.T) {
	t.Parallel()
	r, s, l := directivePropertyWorld(t)
	ctx := context.Background()
	a, _ := r.RecordDirective(ctx, "project", "PRJ", "sup", "INI", l, "a", dref(t, "project_assignment", l, "a", ""))
	_, err := s.DB.Exec("INSERT INTO scope_directives SELECT 'old',scope_kind,scope_key,'next',from_scope_key,link_id,link_kind,digest,reference,revision+1,NULL,NULL,NULL,recorded_at FROM scope_directives WHERE directive_id=?", did(a))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := r.ContestedDirectives(ctx, "project", "PRJ")
	if len(v) != 0 {
		t.Fatal(v)
	}
}
func Test24_DIR_8_HeldReportNamed(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	standing, _ := f.c.Standing(f.ctx, "PRJ-1", nil)
	holds, err := f.c.ReportHolds(f.ctx, standing)
	if err != nil || len(holds) != 0 {
		t.Fatal(holds, err)
	}
}
func Test24_DIR_9_OnlyOwedUnsentHeld(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	_, stage := f.staged(t)
	if _, err := f.s.DB.Exec("DELETE FROM scope_bindings WHERE role='supervisor'"); err != nil {
		t.Fatal(err)
	}
	standing, _ := f.c.Standing(f.ctx, "PRJ-1", nil)
	holds, err := f.c.ReportHolds(f.ctx, standing)
	if err != nil || len(holds) != 1 || holds[0]["reason"] != "unregistered_scope" {
		t.Fatal(holds, err)
	}
	_ = stage
}

func Test24_AUT_1_ObligationStagesAndSendsOnce(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	h := &sendHost{status: "idle"}
	got, err := f.c.AutoSend(f.ctx, h, 1700000000, 4, 2, "", "", "")
	if err != nil || got.SupervisorStaged != 1 || got.SupervisorSent != 1 {
		t.Fatal(got, err)
	}
	again, err := f.c.AutoSend(f.ctx, h, 1700003600, 4, 2, got.AfterProject, got.AfterStagedAt, got.AfterMessageID)
	if err != nil || again.SupervisorSent != 0 {
		t.Fatal(again, err)
	}
}
func Test24_AUT_4_ArchivedWaits(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	h := &sendHost{status: "idle", archived: true}
	got, err := f.c.AutoSend(f.ctx, h, 1700000000, 4, 2, "", "", "")
	if err != nil || got.Deferred < 1 || got.SupervisorSent != 0 {
		t.Fatal(got, err)
	}
}
func Test24_AUT_5_ParentAndAutoConverge(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	h := &sendHost{status: "idle"}
	_, err := f.c.Attempt(f.ctx, stage["messageId"].(string), h, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.c.AutoSend(f.ctx, h, 1700003600, 4, 2, "", "", "")
	if err != nil || got.SupervisorSent != 0 {
		t.Fatal(got, err)
	}
}
func Test24_AUT_7_UnaddressedHeadDoesNotBlock(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	if _, err := f.s.DB.Exec("UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved' WHERE message_id=?", stage["messageId"]); err != nil {
		t.Fatal(err)
	}
	rows, err := f.c.autoHeads(f.ctx, 1700000000, 8, "", "")
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func Test24_SLF_1_SettingsFreeResumeRefusesDrift(t *testing.T) { slfWhole(t, "SLF-1") }
func Test24_SLF_2_PolicyDerivedCarriesSettings(t *testing.T)   { slfWhole(t, "SLF-2") }
func Test24_SLF_3_RootsMayNarrowNeverWiden(t *testing.T)       { slfWhole(t, "SLF-3") }
func Test24_SLF_4_SupervisorResumeSettingsFree(t *testing.T)   { slfWhole(t, "SLF-4") }
func Test24_SLF_5_RenderedProgramAbsolute(t *testing.T)        { slfWhole(t, "SLF-5") }
func Test24_SLF_6_OmissionBytesNameReason(t *testing.T)        { slfWhole(t, "SLF-6") }
func Test24_SLF_7_MistypedRecordRefused(t *testing.T)          { slfWhole(t, "SLF-7") }
func Test24_SLF_8_TextRootsNeverAdmitWider(t *testing.T)       { slfWhole(t, "SLF-8") }
func Test24_SLF_9_MalformedAnswerNamed(t *testing.T)           { slfWhole(t, "SLF-9") }
func Test24_SLF_10_NonTextWritableRoots(t *testing.T)          { slfWhole(t, "SLF-10") }
func Test24_SLF_11_SandboxTypesAreJSONTypes(t *testing.T)      { slfWhole(t, "SLF-11") }
func Test24_SLF_12_EnvironmentRootsDefaultCwd(t *testing.T)    { slfWhole(t, "SLF-12") }
func Test24_SLF_13_PermissionProfileJSONEquality(t *testing.T) { slfWhole(t, "SLF-13") }
func Test24_SLF_14_EnvironmentComparedWhole(t *testing.T)      { slfWhole(t, "SLF-14") }
func Test24_SLF_15_FakeHostUnreadableAnswer(t *testing.T)      { slfWhole(t, "SLF-15") }
