package orm

import (
	"errors"
	"reflect"

	"gopkg.in/pg.v5/internal"
)

func Update(db DB, model interface{}) error {
	res, err := NewQuery(db, model).Update()
	if err != nil {
		return err
	}
	return internal.AssertOneRow(res.RowsAffected())
}

type updateQuery struct {
	*Query
}

var _ QueryAppender = (*updateQuery)(nil)

func (q updateQuery) AppendQuery(b []byte, params ...interface{}) ([]byte, error) {
	var err error

	if len(q.with) > 0 {
		b, err = q.appendWith(b, "")
		if err != nil {
			return nil, err
		}
	}

	b = append(b, "UPDATE "...)
	b = q.appendFirstTable(b)

	b, err = q.mustAppendSet(b)
	if err != nil {
		return nil, err
	}

	if q.hasOtherTables() {
		b = append(b, " FROM "...)
		b = q.appendOtherTables(b)
	}

	b, err = q.mustAppendWhere(b)
	if err != nil {
		return nil, err
	}

	if len(q.returning) > 0 {
		b = q.appendReturning(b)
	}

	return b, nil
}

func (q updateQuery) mustAppendSet(b []byte) ([]byte, error) {
	if len(q.set) > 0 {
		b = q.appendSet(b)
		return b, nil
	}

	if q.model == nil {
		return nil, errors.New("pg: Model(nil)")
	}

	b = append(b, " SET "...)

	table := q.model.Table()
	strct := q.model.Value()

	if fields := q.getFields(); len(fields) > 0 {
		for i, fieldName := range fields {
			field, err := table.GetField(fieldName)
			if err != nil {
				return nil, err
			}

			if i > 0 {
				b = append(b, ", "...)
			}

			b = append(b, field.ColName...)
			b = append(b, " = "...)
			b = q.appendFieldValue(b, field, strct)
		}
		return b, nil
	}

	start := len(b)
	for _, field := range table.Fields {
		if field.Has(PrimaryKeyFlag) {
			continue
		}

		b = append(b, field.ColName...)
		b = append(b, " = "...)
		b = q.appendFieldValue(b, field, strct)
		b = append(b, ", "...)
	}
	if len(b) > start {
		b = b[:len(b)-2]
	}
	return b, nil
}

// appendFieldValue writes the column's value, or a placeholder when this render
// is sanitized. A SET clause is built straight from the model rather than
// through FormatQuery, so it is one of the few places that has to ask.
func (q updateQuery) appendFieldValue(b []byte, field *Field, strct reflect.Value) []byte {
	if q.sanitize {
		return append(b, '?')
	}
	return field.AppendValue(b, strct, 1)
}
