package migrate

// apply_review_followup_test.go holds the CRW-879 cases for the two findings the operator's pair evaluation of the
// state-copy apply left open after CRW-812: the dependency order judged a QA receipt by its file name instead of its
// content, and the write count inferred a completed rename from the destination's bytes. Each case names the behaviour it
// pins and fails on the code before CRW-879 for the reason its name gives.

import (
	"bytes"
	"errors"
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// R1: a receipt under another name still publishes after the artifact its own manifest names, so an interruption between
// the two publications cannot leave the reference dangling.
func TestMigrateApplyReviewFollowupPublishesTheArtifactOfAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if v, i := slices.Index(got, "verdict.json"), slices.Index(got, "a.json"); v < 0 || i < 0 || v > i {
		t.Errorf("the artifact must publish before the receipt that names it: %v", got)
	}
}

// Control: the QA receipt name still takes the referrer's place, for a manifest this run can read and for one it cannot
// judge by content, so the dependencies-first order an unreadable record kept before is unchanged.
func TestMigrateApplyReviewFollowupKeepsTheQaReceiptOrder(t *testing.T) {
	for name, receipt := range map[string]string{
		"readable manifest":          "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
		"manifest of the wrong type": "{\"artifactManifest\":\"verdict.json\"}",
	} {
		t.Run(name, func(t *testing.T) {
			_, r, p := apPlan(t, map[string]string{
				"evidence/s/qa-receipt.json": receipt,
				"evidence/s/verdict.json":    "v",
			}, nil)
			pub, leaves := migrateApplyReviewRenames(t)
			if _, err := applyWith(r, p, pub); err != nil {
				t.Fatal(err)
			}
			got := leaves()
			if v, i := slices.Index(got, "verdict.json"), slices.Index(got, "qa-receipt.json"); v < 0 || i < 0 || v > i {
				t.Errorf("the artifact must publish before the receipt that names it: %v", got)
			}
		})
	}
}

// Control: a directory sync failure after this run's own rename still counts the write and records the sync failure.
func TestMigrateApplyReviewFollowupCountsARenameThatThenLostItsSync(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	seen := 0
	pub.at = func(step string) error {
		if step == "dirsync" {
			if seen++; seen == 2 { // 1 the canonical .gitignore, 2 the session file this run renamed
				return errApplyInterrupted
			}
		}
		return nil
	}
	res, err := applyWith(r, p, pub)
	if !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("a failed directory sync must stop the run: %v", err)
	}
	if res.WritesCompleted != 1 {
		t.Errorf("the rename this run completed must be counted: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); !strings.Contains(ai.Note, "directory sync failed") {
		t.Errorf("the item must record the directory sync failure: %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the final file must be whole: %q", got)
	}
}

// R2: a failure before this run's rename is not this run's write, whatever the destination holds. settle saw nothing, a
// competing writer publishes the same bytes and mode in the window that opens, and this run's own temporary create then
// fails with ENOSPC: nothing of this run's was renamed, so nothing may be counted and no directory sync failed.
func TestMigrateApplyReviewFollowupDoesNotCountAWriteItNeverRenamed(t *testing.T) {
	ws, r, p := apPlan(t, map[string]string{"sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	pub := newPub(t)
	real := pub.createTemp
	seen := 0
	pub.createTemp = func(dir *Dir, name string) (int, error) {
		if seen++; seen == 1 { // 1 the canonical .gitignore, 2 the session file under test
			return real(dir, name)
		}
		put(t, apDst(ws, "sessions/a.json"), "{\"phase\":\"P\"}", 0o644)
		return -1, unix.ENOSPC
	}
	res, err := applyWith(r, p, pub)
	if !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("a failed temporary create must stop the run: %v", err)
	}
	if res.WritesCompleted != 0 {
		t.Errorf("a file this run never renamed must not be counted: %d", res.WritesCompleted)
	}
	if ai := apItem(t, res, "sessions/a.json"); ai.Note != "" {
		t.Errorf("no directory sync failed, so the item must carry no note: %q", ai.Note)
	}
	if got := get(t, apDst(ws, "sessions/a.json")); got != "{\"phase\":\"P\"}" {
		t.Errorf("the competing writer's file must stay: %q", got)
	}
}

// Control: a record under evidence/ that holds no manifest array is not a receipt, so its dependencies are not hoisted and
// the plan order stands. This is the case that keeps the content judgement from reordering every artifact group.
func TestMigrateApplyReviewFollowupKeepsPlanOrderWithoutAManifest(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"verdict\":true}",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if a, v := slices.Index(got, "a.json"), slices.Index(got, "verdict.json"); a < 0 || v < 0 || a > v {
		t.Errorf("a record with no manifest array must keep the plan order: %v", got)
	}
}

// The two halves of the content judgement are pinned apart, because either one alone puts an artifact before the receipt
// that names it and would hide the other reverting to the name test.

// R1a, the ordering key half: the receipt itself is demoted to the referrer's place by its content, so a record under
// another name publishes after an unrelated record of its rank even though plan order puts the receipt first.
func TestMigrateApplyReviewFollowupDemotesAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json": "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/b.json": "plain",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if b, a := slices.Index(got, "b.json"), slices.Index(got, "a.json"); b < 0 || a < 0 || b > a {
		t.Errorf("the receipt under another name must publish after the record of its rank: %v", got)
	}
}

// R1b, the manifest scan half: the artifact a receipt under another name lists is hoisted above a plain record of the same
// rank, which only the scan reading that receipt's content can do.
func TestMigrateApplyReviewFollowupHoistsTheArtifactOfAReceiptUnderAnotherName(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":         "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/b.json":         "plain",
		"evidence/s/z/verdict.json": "v",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	if v, b := slices.Index(got, "verdict.json"), slices.Index(got, "b.json"); v < 0 || b < 0 || v > b {
		t.Errorf("the artifact must be hoisted above the plain record: %v", got)
	}
}

// R1c: a record the receipt reader could not read is not judged by content, so the plan's own measurement decides it
// before anything is opened. The plan's size stands in for the file, so the case needs no record of that size.
func TestMigrateApplyReviewFollowupKeepsTheNameOrderPastTheReceiptReadBound(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/qa-receipt.json": "{\"artifactManifest\":[{\"path\":\"verdict.json\",\"kind\":\"verdict\"}]}",
	}, nil)
	it := apItem(t, &ApplyResult{Items: planItems(p)}, "evidence/s/qa-receipt.json").Item
	a := &applyRun{roots: r, plan: p, dirs: map[string]*Dir{}, srcs: map[string]*Dir{}, made: map[string]bool{}, result: &ApplyResult{}}
	defer a.close()
	if manifest, ok := a.migrateReviewFollowupReceiptManifest(it); !ok || len(manifest) != 1 {
		t.Fatalf("a record within the bound must be judged by content: %v %v", manifest, ok)
	}
	it.Size = migrateReviewFollowupReceiptReadCap + 1
	if manifest, ok := a.migrateReviewFollowupReceiptManifest(it); ok || manifest != nil {
		t.Errorf("a record past the receipt reader's bound must not be judged by content: %v %v", manifest, ok)
	}
}

// R1d: the bound is the bytes of the record itself, not the bytes up to the object's last token. The decoder consumes the
// whitespace after the object, so a record that grew past the bound after the plan measured it would otherwise be judged by
// content while the receipt reader refuses it. The helper takes the limit, so the case needs no record of that size.
func TestMigrateApplyReviewFollowupBoundsTheRecordNotTheObject(t *testing.T) {
	body := "{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}"
	for name, c := range map[string]struct {
		record string
		limit  int64
		want   bool
		// entries is how many of the record's entries name a dependency: a receipt whose entries are all of an
		// unexpected shape is still a receipt, with none.
		entries int
	}{
		"exactly the bound":             {record: body, limit: int64(len(body)), want: true, entries: 1},
		"past the bound by whitespace":  {record: body + "\n", limit: int64(len(body))},
		"past the bound by a long tail": {record: body + strings.Repeat(" ", 8), limit: int64(len(body))},
		"whitespace within the bound":   {record: body + "\n", limit: int64(len(body)) + 1, want: true, entries: 1},
		"truncated below the bound":     {record: body[:len(body)-1], limit: int64(len(body))},
		"data after the object":         {record: body + "{}", limit: int64(len(body)) + 2},
		"no manifest array":             {record: "{\"a\":1}", limit: 8},
		// The receipt reader looks the key up by its exact spelling in a decoded object, so a key that differs only in
		// case is not the manifest it reads, and this order must not treat it as one.
		"key of another case": {record: "{\"ArtifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", limit: 200},
		// The judgement is the array's presence and length, not the shape of its entries: an array whose entries this
		// reader cannot use is still a receipt, so it keeps the referrer's place rather than falling back to plan order.
		"entry of another type":      {record: "{\"artifactManifest\":[1,2]}", limit: 40, want: true},
		"entry of the wrong shape":   {record: "{\"artifactManifest\":[{\"path\":1,\"kind\":2}]}", limit: 60, want: true},
		"one good entry and one bad": {record: "{\"artifactManifest\":[1,{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", limit: 90, want: true, entries: 1},
		// An entry's own keys are read by their exact spelling too, so a field spelled differently names no dependency
		// rather than a false one the receipt reader would never use.
		"entry field of another case": {record: "{\"artifactManifest\":[{\"Path\":\"v.json\",\"Kind\":\"verdict\"}]}", limit: 80, want: true},
		// A duplicated key is read the way the receipt reader's own map decode reads it: the last value wins.
		"duplicate key keeps the last": {
			record:  "{\"artifactManifest\":[{\"path\":\"first.json\",\"kind\":\"verdict\"}],\"artifactManifest\":[{\"path\":\"last.json\",\"kind\":\"verdict\"}]}",
			limit:   200,
			want:    true,
			entries: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, ok := migrateReviewFollowupDecodeManifest(strings.NewReader(c.record), c.limit)
			if ok != c.want {
				t.Errorf("ok = %v, want %v (record %d bytes, limit %d)", ok, c.want, len(c.record), c.limit)
			}
			if ok && len(manifest) != c.entries {
				t.Errorf("manifest = %v, want %d entries", manifest, c.entries)
			}
			if name == "duplicate key keeps the last" && ok && manifest[0].Path != "last.json" {
				t.Errorf("the last value of a duplicated key must win, as the reader's map decode does: %v", manifest)
			}
			if !ok && manifest != nil {
				t.Errorf("a refused record must return no manifest: %v", manifest)
			}
		})
	}
}

func planItems(p *Plan) []ApplyItem {
	items := make([]ApplyItem, len(p.Items))
	for i, it := range p.Items {
		items[i].Item = it
	}
	return items
}

// R1e: a record that is both another receipt's artifact and a receipt itself still publishes after the artifact it names, so
// a chain of receipts is ordered by its dependencies rather than by plan order.
func TestMigrateApplyReviewFollowupOrdersAChainOfReceipts(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		// a.json names z/verdict.json, which is itself a receipt and names z/identity.json beside it: both artifacts are
		// referenced, so only the chain length can order them, and plan order would publish the middle record first.
		"evidence/s/a.json":            "{\"artifactManifest\":[{\"path\":\"z/verdict.json\",\"kind\":\"verdict\"}]}",
		"evidence/s/z/verdict.json":    "{\"artifactManifest\":[{\"path\":\"z/identity.json\",\"kind\":\"artifact-identity\"}]}",
		"evidence/s/z/z/identity.json": "{\"ok\":true}",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	// Plan order is a.json, z/verdict.json, z/z/identity.json; the chain must publish deepest artifact first.
	deep, mid, top := slices.Index(got, "identity.json"), slices.Index(got, "verdict.json"), slices.Index(got, "a.json")
	if deep < 0 || mid < 0 || top < 0 {
		t.Fatalf("the chain must publish: %v", got)
	}
	if deep > mid || mid > top {
		t.Errorf("a chain of receipts must publish deepest artifact first: %v", got)
	}
}

// R1f: the key a cycle gets is the plan's own, not the order a Go map happened to hand the records over. Two records that
// name each other cannot be ordered against one another, but a rerun of the same plan must still publish them the same
// way, or the same input has two orders and neither is the one the report describes.
func TestMigrateApplyReviewFollowupOrdersACycleTheSameWayOnEveryRun(t *testing.T) {
	deps := map[string][]string{"a.json": {"b.json"}, "b.json": {"a.json"}}
	want := migrateReviewFollowupDepths(deps)
	for i := 0; i < 200; i++ {
		if got := migrateReviewFollowupDepths(deps); !maps.Equal(got, want) {
			t.Fatalf("the same plan must give the same key on every run: %v, then %v", want, got)
		}
	}
	// A record that names nothing is still 0, and a chain is still one longer than what it names.
	chain := map[string][]string{"a.json": {"b.json"}, "b.json": {"c.json"}}
	if got := migrateReviewFollowupDepths(chain); got["c.json"] != 0 || got["b.json"] != 1 || got["a.json"] != 2 {
		t.Errorf("a chain must keep its depth: %v", got)
	}
}

// R1g: a record that is not a receipt must be judged without being held. The receipt reader's bound is the size of the
// largest string the oracle could hold, far larger than any record, and this scan looks at every planned file under
// evidence/, so an ordinary artifact of tens of megabytes must cost a bounded pass rather than a copy of itself. A
// record that does mention the manifest key is a receipt the reader itself would read, so it is read the same way.
func TestMigrateApplyReviewFollowupJudgesALargeRecordWithoutHoldingIt(t *testing.T) {
	big := bytes.Repeat([]byte{'x'}, 16<<20)
	notText := bytes.Repeat([]byte{0xff}, 16<<20)
	for name, c := range map[string]struct {
		record []byte
		want   bool
		// bounded is true when the judgement must not hold the record: it cannot be a receipt, so nothing of it needs
		// to be kept. A record that mentions the manifest key is read as the receipt reader reads it.
		bounded bool
	}{
		// A record that cannot be a JSON object at all, refused from its first byte.
		"not text":      {record: notText, bounded: true},
		"not an object": {record: append([]byte("["), big...), bounded: true},
		// Leading whitespace that alone is larger than any probe: the walk must stream it, not buffer it.
		"long leading whitespace": {record: append(bytes.Repeat([]byte{' '}, 8<<20), []byte("{\"a\":1}")...), bounded: true},
		// A whole JSON object with no manifest: valid, but this order holds nothing of it.
		"an object with no manifest": {record: []byte("{\"payload\":\"" + string(big) + "\"}"), bounded: true},
		// A whole JSON object whose manifest is there and large: a receipt, read the way the reader reads it.
		"a receipt with a large manifest": {
			record: []byte("{\"artifactManifest\":[{\"path\":\"" + string(big) + "\",\"kind\":\"verdict\"}]}"),
			want:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			manifest, ok := migrateReviewFollowupDecodeManifest(bytes.NewReader(c.record), migrateReviewFollowupReceiptReadCap)
			runtime.ReadMemStats(&after)
			if ok != c.want {
				t.Errorf("ok = %v, want %v (manifest %v)", ok, c.want, manifest)
			}
			if !c.want && manifest != nil {
				t.Errorf("a refused record must return no manifest: %v", manifest)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; c.bounded && grew > 8<<20 {
				t.Errorf("the judgement allocated %d bytes of a %d byte record", grew, len(c.record))
			}
		})
	}
}

// R1i: the walk must decide exactly what decoding the record would decide. These are the records where a hand-rolled
// reader most easily drifts from encoding/json: an escaped key, a duplicate key, a member of every other JSON type, a
// nested object that also holds the key, braces and brackets inside strings, and a record with data after it.
func TestMigrateApplyReviewFollowupWalksAsTheDecoderWould(t *testing.T) {
	records := map[string]struct {
		record string
		want   bool
	}{
		"a plain manifest":         {"{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		"the member after another": {"{\"a\":1,\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}],\"b\":2}", true},
		"an escaped key":           {"{\"artifact\\u004danifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		// Every spelling of the key that decodes to it must be caught before the record is decoded, or a receipt the
		// reader can read would keep the name judgement instead of its content.
		"a fully escaped first character": {"{\"\\u0061rtifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		// An escape that decodes to a different key is not the manifest key, and the decoder is what decides: this one
		// is \u006d, a lower-case m, so it spells artifactmanifest.
		"an escape spelling another key": {"{\"artifact\\u006danifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", false},
		"a duplicate key, last wins":     {"{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}],\"artifactManifest\":[{\"path\":\"w.json\",\"kind\":\"verdict\"}]}", true},
		"an empty array then a member":   {"{\"artifactManifest\":[],\"artifactManifest\":[{\"path\":\"w.json\",\"kind\":\"verdict\"}]}", true},
		"an empty array":                 {"{\"artifactManifest\":[]}", false},
		"a null member":                  {"{\"artifactManifest\":null}", false},
		"a string member":                {"{\"artifactManifest\":\"x\"}", false},
		"a number member":                {"{\"artifactManifest\":1}", false},
		"an object member":               {"{\"artifactManifest\":{}}", false},
		"a brace inside a string":        {"{\"artifactManifest\":[{\"path\":\"a}\"}]}", true},
		"a bracket inside a string":      {"{\"artifactManifest\":[{\"path\":\"a]\"}]}", true},
		"a nested object holding it":     {"{\"nested\":{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}}", false},
		"nested values before it":        {"{\"outer\":{\"x\":{\"y\":[1,2,{\"z\":\"}\"}]}},\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]}", true},
		"data after the object":          {"{\"artifactManifest\":[{\"path\":\"v.json\",\"kind\":\"verdict\"}]} {}", false},
		"an empty object":                {"{}", false},
		"not an object":                  {"[]", false},
		"not JSON at all":                {"", false},
		"an unterminated array":          {"{\"artifactManifest\":[}", false},
		"a trailing comma":               {"{\"a\":1,}", false},
	}
	for name, c := range records {
		t.Run(name, func(t *testing.T) {
			_, got := migrateReviewFollowupDecodeManifest(strings.NewReader(c.record), migrateReviewFollowupReceiptReadCap)
			if got != c.want {
				t.Errorf("record %s: judged %v, want %v", c.record, got, c.want)
			}
		})
	}
}

// R1g2: the end-to-end case: a receipt whose manifest path holds bytes that are not UTF-8 names the plan file the
// reader's own text holds, so the artifact must publish before a plain record of the same rank. Decoding the raw bytes
// would name a file that is not in the plan and leave the artifact in plan order.
func TestMigrateApplyReviewFollowupReadsAPathAsTheReceiptReaderDoes(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"evidence/s/a.json":                "{\"artifactManifest\":[{\"path\":\"z/identit" + string([]byte{0xe2, 0x82}) + "y.json\",\"kind\":\"artifact-identity\"}]}",
		"evidence/s/b.json":                "{\"plain\":true}",
		"evidence/s/z/identit\uFFFDy.json": "i",
	}, nil)
	pub, leaves := migrateApplyReviewRenames(t)
	if _, err := applyWith(r, p, pub); err != nil {
		t.Fatal(err)
	}
	got := leaves()
	identity, plain := slices.Index(got, "identit\uFFFDy.json"), slices.Index(got, "b.json")
	if identity < 0 || plain < 0 {
		t.Fatalf("both records must publish: %v", got)
	}
	if identity > plain {
		t.Errorf("the reader's own text names the identity file, so it must publish before a plain record: %v", got)
	}
}
