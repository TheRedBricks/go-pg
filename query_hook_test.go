package pg

import (
	"context"
	"testing"
	"time"
)

type recordingHook struct {
	name     string
	order    *[]string
	sawCtx   context.Context
	sawQuery interface{}
	sawErr   error
}

func (h *recordingHook) BeforeQuery(ctx context.Context, ev *QueryEvent) context.Context {
	*h.order = append(*h.order, "before:"+h.name)
	h.sawCtx = ctx
	h.sawQuery = ev.Query
	return ctx
}

func (h *recordingHook) AfterQuery(ctx context.Context, ev *QueryEvent) {
	*h.order = append(*h.order, "after:"+h.name)
	h.sawErr = ev.Error
}

type ctxKey struct{}

// TestQueryHookReceivesWithContext is the whole point of the change: a hook has
// to be able to find the caller's context, because that is where a trace span
// lives and there is no way to recover it after the fact.
func TestQueryHookReceivesWithContext(t *testing.T) {
	var order []string
	hook := &recordingHook{name: "a", order: &order}

	db := &DB{}
	db.AddQueryHook(hook)

	ctx := context.WithValue(context.Background(), ctxKey{}, "the-caller")
	scoped := db.WithContext(ctx)

	hookCtx, event := scoped.beforeQuery("SELECT 1", nil)
	scoped.afterQuery(hookCtx, event, nil, nil)

	if got := hook.sawCtx.Value(ctxKey{}); got != "the-caller" {
		t.Errorf("hook saw ctx value %v, want the-caller", got)
	}
	if hook.sawQuery != "SELECT 1" {
		t.Errorf("hook saw query %v", hook.sawQuery)
	}
}

// TestCopyHelpersCarryHooksAndContext guards the trap this change introduces:
// WithTimeout and WithParam build a DB literal field by field, so a new field
// that is not listed there is silently dropped — and the symptom would be a
// hook that just stops firing after a caller sets a timeout.
func TestCopyHelpersCarryHooksAndContext(t *testing.T) {
	var order []string
	db := &DB{opt: &Options{}, fmter: (&DB{}).fmter}
	db.AddQueryHook(&recordingHook{name: "a", order: &order})
	ctx := context.WithValue(context.Background(), ctxKey{}, "kept")

	for _, tc := range []struct {
		name string
		copy *DB
	}{
		{"WithTimeout", db.WithContext(ctx).WithTimeout(time.Second)},
		{"WithParam", db.WithContext(ctx).WithParam("p", 1)},
		{"WithContext", db.WithContext(ctx)},
	} {
		if len(tc.copy.queryHooks) != 1 {
			t.Errorf("%s dropped the query hooks", tc.name)
		}
		if got := tc.copy.Context().Value(ctxKey{}); got != "kept" {
			t.Errorf("%s dropped the context (got %v)", tc.name, got)
		}
	}
}

func TestHooksRunOutermostLast(t *testing.T) {
	var order []string
	db := &DB{}
	db.AddQueryHook(&recordingHook{name: "outer", order: &order})
	db.AddQueryHook(&recordingHook{name: "inner", order: &order})

	ctx, event := db.beforeQuery("SELECT 1", nil)
	db.afterQuery(ctx, event, nil, nil)

	want := []string{"before:outer", "before:inner", "after:inner", "after:outer"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestNoHooksAllocatesNothing keeps the change free for every caller that has
// not opted in — which is all of them until a hook is registered.
func TestNoHooksAllocatesNothing(t *testing.T) {
	db := &DB{}
	ctx, event := db.beforeQuery("SELECT 1", nil)
	if ctx != nil || event != nil {
		t.Errorf("beforeQuery built an event with no hooks registered")
	}
	db.afterQuery(ctx, event, nil, nil) // must not panic on the nil event
}

func TestUnformattedQueryKeepsPlaceholders(t *testing.T) {
	ev := &QueryEvent{Query: "SELECT * FROM users WHERE email = ?"}
	got, ok := ev.UnformattedQuery()
	if !ok || got != "SELECT * FROM users WHERE email = ?" {
		t.Errorf("UnformattedQuery() = %q, %v", got, ok)
	}

	// A builder query is not raw SQL, and must not be guessed at.
	if _, ok := (&QueryEvent{Query: struct{}{}}).UnformattedQuery(); ok {
		t.Error("UnformattedQuery claimed a non-string query was raw SQL")
	}
}

// TestSiblingCopiesDoNotShareHookStorage is the reviewer's finding: WithContext
// and friends hand their copies the same slice header, so an in-place append
// writes into storage a sibling may also be appending into — and the loser's
// hook is silently overwritten. Sequential setup code is enough to hit it; no
// concurrency required.
func TestSiblingCopiesDoNotShareHookStorage(t *testing.T) {
	var order []string
	base := &DB{}
	// Three first, so the slice has spare capacity for an in-place append to
	// land in — which is exactly what made the clobber possible.
	base.AddQueryHook(&recordingHook{name: "1", order: &order})
	base.AddQueryHook(&recordingHook{name: "2", order: &order})
	base.AddQueryHook(&recordingHook{name: "3", order: &order})

	left := base.WithContext(context.Background())
	right := base.WithContext(context.Background())
	left.AddQueryHook(&recordingHook{name: "left", order: &order})
	right.AddQueryHook(&recordingHook{name: "right", order: &order})

	if got := hookNames(left); got != "1,2,3,left" {
		t.Errorf("left hooks = %s, want 1,2,3,left", got)
	}
	if got := hookNames(right); got != "1,2,3,right" {
		t.Errorf("right hooks = %s, want 1,2,3,right — a sibling overwrote it", got)
	}
	if got := hookNames(base); got != "1,2,3" {
		t.Errorf("base hooks = %s, want 1,2,3 — a copy mutated its parent", got)
	}
}

func hookNames(db *DB) string {
	out := ""
	for i, h := range db.queryHooks {
		if i > 0 {
			out += ","
		}
		out += h.(*recordingHook).name
	}
	return out
}

// TestFailedAcquisitionIsNotObserved pins a LIMITATION, not a desired
// behaviour. The hooks sit in simpleQuery, which is only reached once a
// connection is in hand, so a query that never gets one produces no event at
// all — a hook cannot tell "the pool is exhausted" from "no traffic".
//
// This is the documented cost of hooking at the low-level query path rather
// than at DB.Exec/DB.Query. Change the boundary and this test changes with it;
// it exists so the trade-off is visible rather than discovered.
func TestFailedAcquisitionIsNotObserved(t *testing.T) {
	var order []string
	db := unreachableDB()
	db.AddQueryHook(&recordingHook{name: "h", order: &order})

	_, _ = db.Exec("SELECT 1")

	if len(order) != 0 {
		t.Errorf("hook calls = %v; the boundary moved — update this test and the doc comment on QueryHook", order)
	}
}

// unreachableDB dials a port nothing listens on, so every connection attempt
// fails fast and locally — no live server, no network, no timeout wait.
func unreachableDB() *DB {
	return Connect(&Options{
		Addr:        "127.0.0.1:1",
		User:        "nobody",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  0,
	})
}

type fakeDescriber struct{ op, table string }

func (f fakeDescriber) StatementOperation() string { return f.op }
func (f fakeDescriber) StatementTable() string     { return f.table }

// Statement is how a hook names a Model(...) query without rendering it.
// UnformattedQuery cannot answer for a builder, and FormattedQuery inlines
// every bound value, so before this there was no safe answer at all.
func TestStatementDescribesBuilderQueries(t *testing.T) {
	ev := &QueryEvent{Query: fakeDescriber{op: "SELECT", table: "bookings"}}
	op, table := ev.Statement()
	if op != "SELECT" || table != "bookings" {
		t.Errorf("Statement() = %q, %q; want SELECT, bookings", op, table)
	}

	// Raw SQL is UnformattedQuery's job; Statement must not guess at it.
	op, table = (&QueryEvent{Query: "SELECT * FROM bookings"}).Statement()
	if op != "" || table != "" {
		t.Errorf("Statement() on raw SQL = %q, %q; want empty", op, table)
	}

	// Anything else reports nothing rather than panicking.
	op, table = (&QueryEvent{Query: struct{}{}}).Statement()
	if op != "" || table != "" {
		t.Errorf("Statement() on an unknown query = %q, %q; want empty", op, table)
	}
}
