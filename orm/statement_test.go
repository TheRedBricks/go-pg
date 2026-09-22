package orm

import "testing"

type StatementModel struct {
	Id   int
	Name string
}

// The builder knows its relation; a hook should never have to parse rendered
// SQL to find it, because rendering is what inlines bound values.
func TestStatementDescribesOperationAndTable(t *testing.T) {
	q := NewQuery(nil, &StatementModel{})

	cases := []struct {
		name string
		d    StatementDescriber
		op   string
	}{
		{"select", selectQuery{Query: q}, "SELECT"},
		{"insert", insertQuery{Query: q}, "INSERT"},
		{"update", updateQuery{Query: q}, "UPDATE"},
		{"delete", deleteQuery{Query: q}, "DELETE"},
	}
	for _, c := range cases {
		if got := c.d.StatementOperation(); got != c.op {
			t.Errorf("%s: StatementOperation() = %q, want %q", c.name, got, c.op)
		}
		if got := c.d.StatementTable(); got != "statement_models" {
			t.Errorf("%s: StatementTable() = %q, want statement_models", c.name, got)
		}
	}
}

// The name must come back bare. go-pg stores it quoted, and a span named
// `SELECT "statement_models"` is not what anyone wants to read or group by.
func TestStatementTableIsUnquoted(t *testing.T) {
	sq := selectQuery{Query: NewQuery(nil, &StatementModel{})}
	if got := sq.StatementTable(); got != "statement_models" {
		t.Errorf("StatementTable() = %q, want it unquoted", got)
	}
}

// A query with no model has no relation to report. Returning "" rather than
// guessing keeps a raw q.Table(...) expression — which may carry bound values —
// out of the answer entirely.
func TestStatementTableEmptyWithoutModel(t *testing.T) {
	modelless := selectQuery{Query: NewQuery(nil)}
	if got := modelless.StatementTable(); got != "" {
		t.Errorf("StatementTable() with no model = %q, want empty", got)
	}
	queryless := selectQuery{Query: nil}
	if got := queryless.StatementTable(); got != "" {
		t.Errorf("StatementTable() with no query = %q, want empty", got)
	}

	// An explicitly ignored model must be treated as no model at all.
	q := NewQuery(nil, &StatementModel{})
	q.ignoreModel = true
	ignored := selectQuery{Query: q}
	if got := ignored.StatementTable(); got != "" {
		t.Errorf("StatementTable() with an ignored model = %q, want empty", got)
	}
}
