package orm

import (
	"strings"
	"testing"

	"gopkg.in/pg.v5/types"
)

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

// The whole point: a rendered statement may carry structure and must not carry
// data. Every value below is distinctive so the assertion is "this string does
// not appear anywhere in the output" rather than a shape comparison.
func TestStatementTextSubstitutesNoValues(t *testing.T) {
	secrets := []string{
		"victim@example.com",
		"4111111111111111",
		"tenant-42",
		"P@ssw0rd",
	}

	q := NewQuery(nil, &StatementModel{Id: 99, Name: "victim@example.com"}).
		Where("email = ?", "victim@example.com").
		Where("card = ?", "4111111111111111").
		Where("tenant = ?0", "tenant-42").
		Where("secret = ?", Q("?", "P@ssw0rd"))

	sq := selectQuery{Query: q}
	text, err := sq.StatementText()
	if err != nil {
		t.Fatalf("StatementText: %v", err)
	}
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("rendered statement leaked %q:\n%s", s, text)
		}
	}
	// It has to still be a statement, not an empty string that trivially passes.
	if !strings.Contains(text, "SELECT") || !strings.Contains(text, "statement_models") {
		t.Errorf("rendered statement lost its structure:\n%s", text)
	}
	if !strings.Contains(text, "?") {
		t.Errorf("rendered statement has no placeholders left:\n%s", text)
	}
}

// A named placeholder resolving to a model FIELD is data, and go-pg resolves it
// through the same call that yields the table alias — so the two have to be
// told apart rather than allowed wholesale.
func TestStatementTextKeepsStructureButNotFields(t *testing.T) {
	q := NewQuery(nil, &StatementModel{Id: 7, Name: "confidential"}).
		Where("?TableAlias.name = ?name", nil)

	sq := selectQuery{Query: q}
	text, err := sq.StatementText()
	if err != nil {
		t.Fatalf("StatementText: %v", err)
	}
	if strings.Contains(text, "confidential") {
		t.Errorf("a model field value was rendered:\n%s", text)
	}
	if !strings.Contains(text, "statement_model") {
		t.Errorf("?TableAlias did not render, so the SQL is not usable:\n%s", text)
	}
}

// WithParam values are data too, and they arrive by a different route than
// positional params.
func TestStatementTextIgnoresWithParam(t *testing.T) {
	base := Formatter{}
	f := base.WithParam("tenant", "acme-secret")
	sanitized := f.Sanitized()

	got := string(sanitized.Append(nil, "SELECT * FROM t WHERE x = ?tenant", nil))
	if strings.Contains(got, "acme-secret") {
		t.Errorf("WithParam value leaked: %q", got)
	}

	// The unsanitized formatter must be unchanged — Sanitized copies, not mutates.
	live := string(f.Append(nil, "SELECT * FROM t WHERE x = ?tenant", nil))
	if !strings.Contains(live, "acme-secret") {
		t.Errorf("Sanitized() mutated the original formatter: %q", live)
	}
}

// Rendering must not disturb the query that is about to run.
func TestStatementTextDoesNotMutateTheQuery(t *testing.T) {
	q := NewQuery(nil, &StatementModel{}).Where("email = ?", "real@example.com")

	sq := selectQuery{Query: q}
	if _, err := sq.StatementText(); err != nil {
		t.Fatalf("StatementText: %v", err)
	}

	b, err := selectQuery{Query: q}.AppendQuery(nil)
	if err != nil {
		t.Fatalf("AppendQuery: %v", err)
	}
	if !strings.Contains(string(b), "real@example.com") {
		t.Errorf("the live query stopped substituting values after a sanitized render:\n%s", b)
	}
	if q.sanitize {
		t.Error("sanitize leaked onto the original query")
	}
}

// Identifiers are structure. Sanitizing them away turns a readable statement
// into `ORDER BY ? ?`, which costs the whole point of recording the text.
func TestStatementTextKeepsIdentifiers(t *testing.T) {
	q := NewQuery(nil, &StatementModel{}).
		Where("email = ?", "victim@example.com").
		Order("created_at DESC")

	sq := selectQuery{Query: q}
	text, err := sq.StatementText()
	if err != nil {
		t.Fatalf("StatementText: %v", err)
	}
	if !strings.Contains(text, `ORDER BY "created_at" DESC`) {
		t.Errorf("ORDER BY lost its identifiers:\n%s", text)
	}
	if strings.Contains(text, "victim@example.com") {
		t.Errorf("value leaked alongside the identifiers:\n%s", text)
	}
}

// The leak this caught in production: go-pg loads a has-many relation by
// pre-rendering the parent rows' primary keys and wrapping them in types.Q,
// and it fetches by PK through an appender that never consults the formatter.
// Both routes bypass the placeholder substitution entirely.
func TestStatementTextWithholdsRelationAndPKValues(t *testing.T) {
	// WherePK — the appender that renders values without asking the formatter.
	pkModel := &StatementModel{Id: 4242, Name: "n"}
	q := NewQuery(nil, pkModel)
	q.where = append(q.where, wherePKQuery{q})

	sq := selectQuery{Query: q}
	text, err := sq.StatementText()
	if err != nil {
		t.Fatalf("StatementText: %v", err)
	}
	if strings.Contains(text, "4242") {
		t.Errorf("WherePK leaked the primary key:\n%s", text)
	}
	if !strings.Contains(text, "= ?") {
		t.Errorf("WherePK lost its structure:\n%s", text)
	}
}

// types.Q is how go-pg carries pre-rendered VALUES into a relation join, so it
// must not be waved through as "raw SQL is structure".
func TestStatementTextTreatsRawFragmentsAsValues(t *testing.T) {
	q := NewQuery(nil, &StatementModel{}).
		Where(`(?) IN (?)`, types.Q(`"t"."parent_id"`), types.Q(`('devpkg00000000000001')`))

	sq := selectQuery{Query: q}
	text, err := sq.StatementText()
	if err != nil {
		t.Fatalf("StatementText: %v", err)
	}
	if strings.Contains(text, "devpkg00000000000001") {
		t.Errorf("a types.Q value leaked:\n%s", text)
	}
}

// ...except a sort direction, which is a closed set and keeps ORDER BY useful.
func TestStatementTextKeepsSortDirection(t *testing.T) {
	q := NewQuery(nil, &StatementModel{}).Order("created_at DESC")
	sq := selectQuery{Query: q}
	text, err := sq.StatementText()
	if err != nil {
		t.Fatalf("StatementText: %v", err)
	}
	if !strings.Contains(text, `ORDER BY "created_at" DESC`) {
		t.Errorf("sort direction was elided:\n%s", text)
	}
}

type SecretModel struct {
	Id       int
	Email    string
	Mobile   string
	UserData string
}

// The catch-all. Every statement kind, one model full of sentinels, one
// assertion: none of them may appear in any rendered statement.
//
// This is the test that was missing. SELECT was covered and passed while UPDATE
// and INSERT were exporting whole rows, because a SET clause and a VALUES list
// are built straight from the model and never reach the substitution loop.
func TestStatementTextLeaksNothingForAnyStatementKind(t *testing.T) {
	secrets := []string{
		"victim@example.com",
		"60123456789",
		`{"name":"Devstack admin","email":"dev+admin@mhub.ninja"}`,
		"9182736455",
	}
	model := &SecretModel{
		Id:       9182736455,
		Email:    "victim@example.com",
		Mobile:   "60123456789",
		UserData: `{"name":"Devstack admin","email":"dev+admin@mhub.ninja"}`,
	}

	for _, tc := range []struct {
		kind  string
		build func(*Query) StatementDescriber
	}{
		{"select", func(q *Query) StatementDescriber { return selectQuery{Query: q} }},
		{"insert", func(q *Query) StatementDescriber { return insertQuery{Query: q} }},
		{"update", func(q *Query) StatementDescriber { return updateQuery{Query: q} }},
		{"delete", func(q *Query) StatementDescriber { return deleteQuery{Query: q} }},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			q := NewQuery(nil, model).Where("email = ?", "victim@example.com")
			q.where = append(q.where, wherePKQuery{q})

			text, err := tc.build(q).StatementText()
			if err != nil {
				t.Fatalf("StatementText: %v", err)
			}
			for _, s := range secrets {
				if strings.Contains(text, s) {
					t.Errorf("%s leaked %q:\n%s", tc.kind, s, text)
				}
			}
			if text == "" {
				t.Errorf("%s rendered nothing, so the assertion above is vacuous", tc.kind)
			}
		})
	}
}

// UPDATE and DELETE with no explicit Where take a different path: the builder
// synthesises `WHERE pk = <value>` from the model. That path passed its
// formatter as nil, so the sanitize flag was invisible and the primary key was
// rendered — caught in a live span after the SET clause had already been fixed.
func TestStatementTextWithholdsTheImplicitPrimaryKey(t *testing.T) {
	const pk = "daovj2dg3nls73bnkdh0"

	for _, tc := range []struct {
		kind  string
		build func(*Query) StatementDescriber
	}{
		{"update", func(q *Query) StatementDescriber { return updateQuery{Query: q} }},
		{"delete", func(q *Query) StatementDescriber { return deleteQuery{Query: q} }},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			// No Where at all: this is what makes the builder synthesise one.
			q := NewQuery(nil, &StringPKModel{Id: pk, Name: "n"})

			text, err := tc.build(q).StatementText()
			if err != nil {
				t.Fatalf("StatementText: %v", err)
			}
			if strings.Contains(text, pk) {
				t.Errorf("%s leaked the implicit primary key:\n%s", tc.kind, text)
			}
			if !strings.Contains(text, "WHERE") {
				t.Errorf("%s lost its WHERE clause entirely:\n%s", tc.kind, text)
			}
		})
	}
}

type StringPKModel struct {
	Id   string
	Name string
}
