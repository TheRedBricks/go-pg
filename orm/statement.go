package orm

import "strings"

// StatementDescriber is implemented by every query builder this package hands
// to the DB. It lets a query hook name a statement — "SELECT bookings" — from
// the builder itself.
//
// Note what is deliberately absent: there is no StatementText here, and unlike
// v5 there cannot be. This version formats each clause as the caller builds it
// — Where() runs FormatQuery immediately and appends the result to a []byte —
// so by the time a hook sees the query its bound values are already rendered
// into those bytes and the template is gone. Reproducing the placeholder form
// would mean recording every clause twice for every query in the process,
// traced or not. The operation and the relation need no rendering at all, so
// they are what this exposes.
type StatementDescriber interface {
	// StatementOperation returns the SQL verb: SELECT, INSERT, UPDATE, DELETE.
	StatementOperation() string
	// StatementTable returns the relation the statement targets, or "" when the
	// builder has no model.
	StatementTable() string
}

// statementTable reads the relation off the builder's model. It never touches
// q.tables: in this version that field is a pre-rendered []byte which may hold
// bound values, and returning any of it would leak them.
func statementTable(q *Query) string {
	if q == nil || q.model == nil {
		return ""
	}
	t := q.model.Table()
	if t == nil {
		return ""
	}
	return strings.Trim(string(t.Name), `"`)
}

func (q selectQuery) StatementOperation() string { return "SELECT" }
func (q selectQuery) StatementTable() string     { return statementTable(q.Query) }

func (q insertQuery) StatementOperation() string { return "INSERT" }
func (q insertQuery) StatementTable() string     { return statementTable(q.Query) }

func (q updateQuery) StatementOperation() string { return "UPDATE" }
func (q updateQuery) StatementTable() string     { return statementTable(q.Query) }

func (q deleteQuery) StatementOperation() string { return "DELETE" }
func (q deleteQuery) StatementTable() string     { return statementTable(q.Query) }

var (
	_ StatementDescriber = (*selectQuery)(nil)
	_ StatementDescriber = (*insertQuery)(nil)
	_ StatementDescriber = (*updateQuery)(nil)
	_ StatementDescriber = (*deleteQuery)(nil)
)
