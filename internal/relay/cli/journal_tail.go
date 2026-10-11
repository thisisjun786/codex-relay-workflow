package cli

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// journal-tail (CRW-1194, R9) is the relay's awaitable feed: the journal table read in seq order
// after a cursor, as crw-journal-tail/1 (docs/relay/journal-tail.md). The format lives in that
// document and in the types below, not in contract/schema.

// JournalTailSchema names the answer's format.
const JournalTailSchema = "crw-journal-tail/1"

const (
	journalTailDefaultLimit = 100
	journalTailMaxLimit     = 1000
	journalTailMaxWait      = 3600
	journalTailMaxKinds     = 64
)

// journalTailPoll is the pause between two reads of a waiting journal-tail: one second.
var journalTailPoll = time.Second

// JournalTailEntry is one journal row. Subject and Detail are nil where the row has none; Detail is
// the stored text as it is (the journal's details are JSON documents, but the column is text).
type JournalTailEntry struct {
	Seq     int64
	At      string
	Kind    string
	Subject *string
	Detail  *string
}

// JournalTailPage is one answer of journal-tail.
type JournalTailPage struct {
	// After is the cursor the read started from.
	After int64
	// Kinds are the kinds the read was limited to; nil reads every kind.
	Kinds   []string
	Entries []JournalTailEntry
	// Cursor is the seq of the last entry returned, After when there is none: the next read's --after.
	Cursor int64
	// More is true when the page stopped at the limit with rows still to read.
	More bool
}

// Answer is the page as the command prints it.
func (p JournalTailPage) Answer() contract.OrderedObject {
	var kinds any
	if p.Kinds != nil {
		list := make([]any, len(p.Kinds))
		for i, kind := range p.Kinds {
			list[i] = kind
		}
		kinds = list
	}
	entries := make([]any, len(p.Entries))
	for i, e := range p.Entries {
		entries[i] = contract.OrderedObject{
			{Key: "seq", Value: e.Seq}, {Key: "at", Value: e.At}, {Key: "kind", Value: e.Kind},
			{Key: "subject", Value: tailNullable(e.Subject)}, {Key: "detail", Value: tailNullable(e.Detail)},
		}
	}
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: JournalTailSchema},
		{Key: "after", Value: p.After}, {Key: "kinds", Value: kinds},
		{Key: "cursor", Value: p.Cursor}, {Key: "more", Value: p.More}, {Key: "entries", Value: entries},
	}
}

func tailNullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

var journalTailCommand = dispatch.Command{Name: "journal-tail", ReadOnly: true, Run: runJournalTail}

func init() { dispatch.Register(nil, journalTailCommand) }

// openJournalTailStore is the store journal-tail reads through: the read-only opener (mode=ro,
// query_only), never the writable one a read-only command would otherwise be handed for a store
// this runtime may write. It creates nothing: a state with no store is store_absent.
func openJournalTailStore(ctx context.Context, path string) (*store.Store, error) {
	// the absent and the partial store are judged on the resolved database as every read-only command judges them
	if err := store.CheckReadable(path); err != nil {
		return nil, err
	}
	return store.OpenReadOnlyStore(ctx, path)
}

func tailUsage(detail string) error {
	return &dispatch.UsageError{Detail: detail, Code: contract.ExitUsage}
}

// journalTailKinds reads --kinds: comma separated kinds, none of them empty.
func journalTailKinds(text string) ([]string, error) {
	var kinds []string
	for _, kind := range strings.Split(text, ",") {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			return nil, tailUsage("--kinds is a comma separated list of journal kinds, none of them empty")
		}
		duplicate := false
		for _, seen := range kinds {
			duplicate = duplicate || seen == kind
		}
		if !duplicate {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) > journalTailMaxKinds {
		return nil, tailUsage("--kinds names at most 64 kinds")
	}
	return kinds, nil
}

func runJournalTail(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	after := args.Integer("after")
	if after < 0 {
		return nil, tailUsage("--after is a journal seq, 0 or more")
	}
	limit := int64(journalTailDefaultLimit)
	if args.Given("limit") {
		if limit = args.Integer("limit"); limit < 1 || limit > journalTailMaxLimit {
			return nil, tailUsage("--limit is between 1 and 1000")
		}
	}
	wait := int64(0)
	if args.Given("wait") {
		if wait = args.Integer("wait"); wait < 0 || wait > journalTailMaxWait {
			return nil, tailUsage("--wait is between 0 and 3600 seconds")
		}
	}
	var kinds []string
	if args.Given("kinds") {
		var err error
		if kinds, err = journalTailKinds(args.Text("kinds")); err != nil {
			return nil, err
		}
	}
	opened, err := openJournalTailStore(ctx, services.Selection.DBPath())
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	page, err := TailJournal(ctx, opened, after, kinds, int(limit), time.Duration(wait)*time.Second)
	if err != nil {
		return nil, err
	}
	return page.Answer(), nil
}

// TailJournal reads the journal rows after seq after, at most limit of them, in seq order, limited to
// kinds when there are any. It reads once; with wait it reads again every journalTailPoll while the
// read is empty and the wait is not over. Each read is one short statement outside any transaction
// of ours, and nothing is held while it sleeps.
func TailJournal(ctx context.Context, s *store.Store, after int64, kinds []string, limit int, wait time.Duration) (JournalTailPage, error) {
	deadline := time.Now().Add(wait)
	for {
		page, err := readJournal(ctx, s, after, kinds, limit)
		if err != nil || len(page.Entries) > 0 || wait <= 0 {
			return page, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return page, nil
		}
		pause := min(journalTailPoll, remaining)
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return page, ctx.Err()
		case <-timer.C:
		}
	}
}

// readJournal is one read of the journal rows after a seq: the primary key's range, or the kind index's
// where kinds limit it, ordered by seq, one row past the limit to learn whether more follow.
func readJournal(ctx context.Context, s *store.Store, after int64, kinds []string, limit int) (JournalTailPage, error) {
	query := "SELECT seq, at, kind, subject, detail FROM journal WHERE seq > ?"
	params := []any{after}
	if len(kinds) > 0 {
		query += " AND kind IN (?" + strings.Repeat(",?", len(kinds)-1) + ")"
		for _, kind := range kinds {
			params = append(params, kind)
		}
	}
	query += " ORDER BY seq LIMIT ?"
	params = append(params, limit+1)
	rows, err := s.Q(ctx).QueryContext(ctx, query, params...)
	if err != nil {
		return JournalTailPage{}, err
	}
	page := JournalTailPage{After: after, Kinds: kinds, Cursor: after}
	for rows.Next() {
		var e JournalTailEntry
		var subject, detail, at, kind any
		if err := rows.Scan(&e.Seq, &at, &kind, &subject, &detail); err != nil {
			_ = rows.Close()
			return JournalTailPage{}, err
		}
		e.At, e.Kind, e.Subject, e.Detail = tailText(at), tailText(kind), tailOptional(subject), tailOptional(detail)
		if len(page.Entries) == limit {
			page.More = true
			break
		}
		page.Entries = append(page.Entries, e)
		page.Cursor = e.Seq
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return JournalTailPage{}, err
	}
	return page, nil
}

func tailText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return ""
}

func tailOptional(v any) *string {
	if v == nil {
		return nil
	}
	s := tailText(v)
	return &s
}
