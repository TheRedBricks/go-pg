package pg

import (
	"context"
	"time"

	"gopkg.in/pg.v4/types"
)

// QueryEvent describes one statement sent to the server. It is passed to every
// registered QueryHook, before the query runs and again after it returns.
type QueryEvent struct {
	StartTime time.Time
	DB        *DB
	// Query is what the caller passed: a string for raw SQL, or the orm's query
	// builder for Model(...) calls.
	Query  interface{}
	Params []interface{}
	// Result and Error are only set by the time AfterQuery is called.
	Result *types.Result
	Error  error
}

// UnformattedQuery returns the statement as the caller wrote it, with its
// placeholders intact, and false when the caller used the query builder rather
// than raw SQL.
//
// Placeholders are deliberately NOT substituted. The formatted statement
// inlines every bound value, so for anything that leaves this process — a span
// attribute, a log line shipped off the box — this is the form to use.
func (ev *QueryEvent) UnformattedQuery() (string, bool) {
	q, ok := ev.Query.(string)
	return q, ok
}

// FormattedQuery returns the statement exactly as it went to the server.
//
// ⚠️ This INLINES every bound parameter, so it contains whatever the caller
// passed — ids, emails, whole row payloads. Safe for local debugging; not safe
// to put on a span or ship anywhere. Prefer UnformattedQuery for those.
func (ev *QueryEvent) FormattedQuery() (string, error) {
	b, err := appendQuery(nil, ev.Query, ev.Params...)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// QueryHook observes every statement this DB runs.
//
// BeforeQuery's returned context is handed back to AfterQuery, which is how a
// hook carries state — a span, a start time — across the two calls without a
// map keyed on anything.
type QueryHook interface {
	BeforeQuery(context.Context, *QueryEvent) context.Context
	AfterQuery(context.Context, *QueryEvent)
}

// AddQueryHook registers a hook. Call it during setup, before the DB serves
// traffic: the hook slice is read without synchronisation on the query path, so
// registering while queries are in flight is a data race.
func (db *DB) AddQueryHook(hook QueryHook) {
	db.queryHooks = append(db.queryHooks, hook)
}

// WithContext returns a DB that runs its queries with ctx, sharing the
// underlying connection pool.
//
// ⚠️ This carries the context to QueryHooks and nothing else. It does NOT make
// queries cancellable: the pool and the network reads underneath predate
// context entirely, so cancelling ctx will not abort a statement already in
// flight. It exists so a hook can find the caller's trace span, which is the
// one thing that cannot be recovered after the fact.
func (db *DB) WithContext(ctx context.Context) *DB {
	return &DB{
		ctx:        ctx,
		opt:        db.opt,
		pool:       db.pool,
		queryHooks: db.queryHooks,
	}
}

// Context returns the context this DB runs queries with, or context.Background
// when none was set.
func (db *DB) Context() context.Context {
	if db.ctx == nil {
		return context.Background()
	}
	return db.ctx
}

// beforeQuery runs the registered hooks and returns the context AfterQuery must
// be given. It allocates nothing when no hook is registered, which is the case
// for every caller that has not opted in.
func (db *DB) beforeQuery(query interface{}, params []interface{}) (context.Context, *QueryEvent) {
	if db == nil || len(db.queryHooks) == 0 {
		return nil, nil
	}
	event := &QueryEvent{
		StartTime: time.Now(),
		DB:        db,
		Query:     query,
		Params:    params,
	}
	ctx := db.Context()
	for _, hook := range db.queryHooks {
		if next := hook.BeforeQuery(ctx, event); next != nil {
			ctx = next
		}
	}
	return ctx, event
}

// afterQuery completes the event and runs the hooks in reverse, so a hook that
// opened something in BeforeQuery closes it before the hook outside it does.
func (db *DB) afterQuery(ctx context.Context, event *QueryEvent, res *types.Result, err error) {
	if event == nil {
		return
	}
	event.Result = res
	event.Error = err
	for i := len(db.queryHooks) - 1; i >= 0; i-- {
		db.queryHooks[i].AfterQuery(ctx, event)
	}
}
