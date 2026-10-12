package orm

import (
	"strings"
	"testing"
)

type StatementModel struct {
	Id     int
	Email  string
	Mobile string
}

// The builder knows its relation without rendering anything, which is the whole
// reason this exists: rendering is where bound values enter the picture.
func TestStatementDescribesOperationAndTable(t *testing.T) {
	q := NewQuery(nil, &StatementModel{})

	for _, tc := range []struct {
		name string
		d    StatementDescriber
		op   string
	}{
		{"select", selectQuery{Query: q}, "SELECT"},
		{"insert", insertQuery{Query: q}, "INSERT"},
		{"update", updateQuery{Query: q}, "UPDATE"},
		{"delete", deleteQuery{Query: q}, "DELETE"},
	} {
		if got := tc.d.StatementOperation(); got != tc.op {
			t.Errorf("%s: StatementOperation() = %q, want %q", tc.name, got, tc.op)
		}
		if got := tc.d.StatementTable(); got != "statement_models" {
			t.Errorf("%s: StatementTable() = %q, want statement_models", tc.name, got)
		}
	}
}

// Bare, not `"statement_models"`: a span named SELECT "statement_models" is not
// what anyone wants to read or group by.
func TestStatementTableIsUnquoted(t *testing.T) {
	sq := selectQuery{Query: NewQuery(nil, &StatementModel{})}
	if got := sq.StatementTable(); got != "statement_models" {
		t.Errorf("StatementTable() = %q, want it unquoted", got)
	}
}

func TestStatementTableEmptyWithoutModel(t *testing.T) {
	modelless := selectQuery{Query: NewQuery(nil)}
	if got := modelless.StatementTable(); got != "" {
		t.Errorf("StatementTable() with no model = %q, want empty", got)
	}
	queryless := selectQuery{Query: nil}
	if got := queryless.StatementTable(); got != "" {
		t.Errorf("StatementTable() with no query = %q, want empty", got)
	}
}

// This version formats each clause as the caller builds it, so q.where already
// holds `(email = 'victim@example.com')` by the time any hook runs. The whole
// safety property here is that the describer reports the MODEL's table and
// never reaches into those bytes.
func TestStatementTableNeverExposesRenderedClauses(t *testing.T) {
	const secret = "victim@example.com"

	q := NewQuery(nil, &StatementModel{Id: 7, Email: secret}).
		Where("email = ?", secret).
		Where("mobile = ?", "60123456789")

	// Precondition: confirm the value really is sitting in the built query, so
	// this test is exercising the risk rather than assuming it away.
	if !strings.Contains(string(q.where), secret) {
		t.Fatalf("precondition failed: expected the value to be pre-rendered into q.where, got %q", q.where)
	}

	sq := selectQuery{Query: q}
	for _, got := range []string{sq.StatementTable(), sq.StatementOperation()} {
		if strings.Contains(got, secret) || strings.Contains(got, "60123456789") {
			t.Errorf("describer returned rendered clause content: %q", got)
		}
	}
	if sq.StatementTable() != "statement_models" {
		t.Errorf("StatementTable() = %q, want statement_models", sq.StatementTable())
	}
}

func TestUnquoteIdentifierPreservesSchemaQualification(t *testing.T) {
	if got := unquoteIdentifier(`"billing"."invoices"`); got != "billing.invoices" {
		t.Fatalf("unquoteIdentifier() = %q, want billing.invoices", got)
	}
}
