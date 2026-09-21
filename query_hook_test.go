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
