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
	// StatementText renders the statement as a template — full SQL structure,
	// every bound value left as its placeholder. See sanitizedText.
	StatementText() (string, error)
}

// sanitizedText renders a builder with value substitution turned off.
//
// The copy is the point: sanitize is set on a throwaway *Query so the original
// — the one that will actually run — is untouched. Rendering reads the query,
// so a shallow copy is enough; nothing here mutates the shared slices.
func sanitizedText(q *Query, build func(*Query) QueryAppender) (string, error) {
	if q == nil {
		return "", nil
	}
	cp := *q
	cp.sanitize = true
	b, err := build(&cp).AppendQuery(nil)
	if err != nil {
		return "", err
	}
	return string(b), nil
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
func (q selectQuery) StatementText() (string, error) {
	return sanitizedText(q.Query, func(cp *Query) QueryAppender {
		return selectQuery{Query: cp, count: q.count}
	})
}

func (q insertQuery) StatementOperation() string { return "INSERT" }
func (q insertQuery) StatementTable() string     { return statementTable(q.Query) }
func (q insertQuery) StatementText() (string, error) {
	return sanitizedText(q.Query, func(cp *Query) QueryAppender {
		return insertQuery{Query: cp, returningFields: q.returningFields}
	})
}

func (q updateQuery) StatementOperation() string { return "UPDATE" }
func (q updateQuery) StatementTable() string     { return statementTable(q.Query) }
func (q updateQuery) StatementText() (string, error) {
	return sanitizedText(q.Query, func(cp *Query) QueryAppender { return updateQuery{Query: cp} })
}

func (q deleteQuery) StatementOperation() string { return "DELETE" }
func (q deleteQuery) StatementTable() string     { return statementTable(q.Query) }
func (q deleteQuery) StatementText() (string, error) {
	return sanitizedText(q.Query, func(cp *Query) QueryAppender { return deleteQuery{Query: cp} })
}

var (
	_ StatementDescriber = (*selectQuery)(nil)
	_ StatementDescriber = (*insertQuery)(nil)
	_ StatementDescriber = (*updateQuery)(nil)
	_ StatementDescriber = (*deleteQuery)(nil)
)
