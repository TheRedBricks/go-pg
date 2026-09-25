package pg

import (
	"context"
	"errors"
	"time"

	"gopkg.in/pg.v5/orm"
	"gopkg.in/pg.v5/types"
)

// QueryEvent describes one logical query operation. It is passed to every
// registered QueryHook before the operation starts and again after its final
// result is known, including connection acquisition, retries, and the
// row-scanning Model.AfterQuery callback.
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

	prepared bool
}

// UnformattedQuery returns the statement as the caller wrote it, with its
// placeholders intact, and false when the caller used the query builder rather
// than raw SQL.
//
// Placeholders are deliberately NOT substituted. The formatted statement
// inlines every bound value, so for anything that leaves this process — a span
// attribute, a log line shipped off the box — this is the form to use.
// Model(...) queries return false; use StatementText for their parameterised
// SQL template and never fall back to FormattedQuery for exported telemetry.
func (ev *QueryEvent) UnformattedQuery() (string, bool) {
	q, ok := ev.Query.(string)
	return q, ok
}

// Statement returns the SQL verb and the target relation for a query built with
// Model(...), and two empty strings for raw SQL — use UnformattedQuery for that.
//
// This is the builder's own answer, not a parse of rendered SQL, and it is the
// only way to learn what a Model query touches without rendering it: rendering
// substitutes every bound parameter, so the rendered text of a Model query
// carries the ids and addresses a span attribute must not. Both values here are
// safe to export — a keyword and an identifier declared in a struct tag.
func (ev *QueryEvent) Statement() (operation, table string) {
	d, ok := ev.Query.(orm.StatementDescriber)
	if !ok {
		return "", ""
	}
	return d.StatementOperation(), d.StatementTable()
}

// StatementText renders a builder query as a template: full SQL structure with
// every bound value left as its placeholder, so it is safe on a span. Returns
// "" for raw SQL — UnformattedQuery already has that, unrendered and therefore
// safer still.
//
// Contrast FormattedQuery, which substitutes every value and must never leave
// the process.
func (ev *QueryEvent) StatementText() string {
	d, ok := ev.Query.(orm.StatementDescriber)
	if !ok {
		return ""
	}
	text, err := d.StatementText()
	if err != nil {
		return ""
	}
	return text
}

// FormattedQuery returns the statement exactly as it went to the server for
// raw and query-builder operations.
//
// ⚠️ This INLINES every bound parameter, so it contains whatever the caller
// passed — ids, emails, whole row payloads. Safe for local debugging; not safe
// to put on a span or ship anywhere. Prefer UnformattedQuery for those.
//
// PostgreSQL prepared statements use $1-style placeholders, which the query
// formatter cannot substitute. FormattedQuery returns an error for those
// events rather than returning unchanged SQL and claiming it was formatted.
func (ev *QueryEvent) FormattedQuery() (string, error) {
	if ev.prepared && len(ev.Params) > 0 {
		return "", errors.New("pg: cannot format PostgreSQL prepared-statement parameters")
	}
	if ev.DB == nil {
		return "", errors.New("pg: QueryEvent has no DB")
	}
	b, err := appendQuery(nil, ev.DB.fmter, ev.Query, ev.Params...)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// QueryHook observes logical Exec and Query operations run through DB, Tx, and
// Stmt. One before/after pair covers connection acquisition, retries, backoff,
// callbacks, and the final result or error.
//
// CopyFrom, CopyTo, LISTEN/UNLISTEN, statement preparation, and internal
// session setup are not observed.
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
//
// Registration allocates fresh storage rather than appending in place. WithContext
// and friends hand their copies the same slice header, so an in-place append writes
// into storage a sibling may also be appending into — and the loser's hook is
// silently overwritten, even when both registrations are sequential setup code.
func (db *DB) AddQueryHook(hook QueryHook) {
	hooks := make([]QueryHook, len(db.queryHooks), len(db.queryHooks)+1)
	copy(hooks, db.queryHooks)
	db.queryHooks = append(hooks, hook)
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
		fmter:      db.fmter,
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

func (db *DB) beforePreparedQuery(query string, params []interface{}) (context.Context, *QueryEvent) {
	ctx, event := db.beforeQuery(query, params)
	if event != nil {
		event.prepared = true
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
