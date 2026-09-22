package orm

import "strings"

// StatementDescriber is implemented by every query builder this package hands
// to the DB. It lets a query hook name a statement — "SELECT bookings" — from
// the builder itself.
//
// This exists because the alternative is rendering the builder to SQL, and
// rendering substitutes every bound parameter: the rendered text of a Model
// query carries the very ids and addresses that a span attribute must not. The
// operation is a keyword and the relation is an identifier declared in a Go
// struct tag, so both are safe to export and bounded in cardinality.
type StatementDescriber interface {
	// StatementOperation returns the SQL verb: SELECT, INSERT, UPDATE, DELETE.
	StatementOperation() string
	// StatementTable returns the relation the statement targets, or "" when the
	// builder has no model (a raw q.Table(...) query, or an ignored model).
	StatementTable() string
}

// statementTable reads the relation off the builder's model. Only the model's
// own table is reported: entries in q.tables are arbitrary FormatAppenders that
// may carry bound values, which is exactly what this is designed not to leak.
func statementTable(q *Query) string {
	if q == nil || !q.hasModel() {
		return ""
	}
	t := q.model.Table()
	if t == nil {
		return ""
	}
	return unquoteIdentifier(string(t.Name))
}

// unquoteIdentifier strips the double quotes go-pg wraps identifiers in, so the
// caller gets `bookings` rather than `"bookings"`.
func unquoteIdentifier(s string) string {
	return strings.Trim(s, `"`)
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
