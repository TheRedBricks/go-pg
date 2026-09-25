package orm

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/pg.v5/internal/parser"
	"gopkg.in/pg.v5/types"
)

type FormatAppender interface {
	AppendFormat([]byte, QueryFormatter) []byte
}

type sepFormatAppender interface {
	FormatAppender
	AppendSep([]byte) []byte
}

//------------------------------------------------------------------------------

type queryParamsAppender struct {
	query  string
	params []interface{}
}

var _ FormatAppender = (*queryParamsAppender)(nil)

func Q(query string, params ...interface{}) FormatAppender {
	return queryParamsAppender{query, params}
}

func (q queryParamsAppender) AppendFormat(b []byte, f QueryFormatter) []byte {
	return f.FormatQuery(b, q.query, q.params...)
}

//------------------------------------------------------------------------------

type whereAppender struct {
	conj   string
	query  string
	params []interface{}
}

var _ FormatAppender = (*whereAppender)(nil)

func (q whereAppender) AppendSep(b []byte) []byte {
	return append(b, q.conj...)
}

func (q whereAppender) AppendFormat(b []byte, f QueryFormatter) []byte {
	b = append(b, '(')
	b = f.FormatQuery(b, q.query, q.params...)
	b = append(b, ')')
	return b
}

//------------------------------------------------------------------------------

type fieldAppender struct {
	field string
}

var _ FormatAppender = (*fieldAppender)(nil)

func (a fieldAppender) AppendFormat(b []byte, f QueryFormatter) []byte {
	return types.AppendField(b, a.field, 1)
}

//------------------------------------------------------------------------------

type Formatter struct {
	namedParams map[string]interface{}
	// sanitize renders the statement as a template: every bound VALUE is left as
	// its placeholder instead of being substituted. It is what makes a rendered
	// query safe to put on a span or a log line. See Sanitized.
	sanitize bool
}

// Sanitized returns a copy of this formatter that renders placeholders instead
// of values. Structure — table names, aliases, column lists — still renders, so
// the result is the statement's shape with none of its data.
func (f Formatter) Sanitized() Formatter {
	cp := f.Copy()
	cp.sanitize = true
	return cp
}

func (f Formatter) String() string {
	if len(f.namedParams) == 0 {
		return ""
	}

	var keys []string
	for k, _ := range f.namedParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var ss []string
	for _, k := range keys {
		ss = append(ss, fmt.Sprintf("%s=%v", k, f.namedParams[k]))
	}
	return " " + strings.Join(ss, " ")
}

func (f Formatter) Copy() Formatter {
	cp := Formatter{sanitize: f.sanitize}
	for param, value := range f.namedParams {
		cp.SetParam(param, value)
	}
	return cp
}

func (f *Formatter) SetParam(param string, value interface{}) {
	if f.namedParams == nil {
		f.namedParams = make(map[string]interface{})
	}
	f.namedParams[param] = value
}

func (f *Formatter) WithParam(param string, value interface{}) Formatter {
	cp := f.Copy()
	cp.SetParam(param, value)
	return cp
}

func (f Formatter) Append(dst []byte, src string, params ...interface{}) []byte {
	if (params == nil && f.namedParams == nil) || strings.IndexByte(src, '?') == -1 {
		return append(dst, src...)
	}
	return f.append(dst, parser.NewString(src), params)
}

func (f Formatter) AppendBytes(dst, src []byte, params ...interface{}) []byte {
	if (params == nil && f.namedParams == nil) || bytes.IndexByte(src, '?') == -1 {
		return append(dst, src...)
	}
	return f.append(dst, parser.New(src), params)
}

func (f Formatter) FormatQuery(dst []byte, query string, params ...interface{}) []byte {
	return f.Append(dst, query, params...)
}

func (f Formatter) append(dst []byte, p *parser.Parser, params []interface{}) []byte {
	var paramsIndex int
	var namedParams *tableParams
	var namedParamsInit bool
	var model tableModel

	if len(params) > 0 {
		var ok bool
		model, ok = params[len(params)-1].(tableModel)
		if ok {
			params = params[:len(params)-1]
		}
	}

	for p.Valid() {
		b, ok := p.ReadSep('?')
		if !ok {
			dst = append(dst, b...)
			continue
		}
		if len(b) > 0 && b[len(b)-1] == '\\' {
			dst = append(dst, b[:len(b)-1]...)
			dst = append(dst, '?')
			continue
		}
		dst = append(dst, b...)

		if id, numeric := p.ReadIdentifier(); id != "" {
			if numeric {
				idx, err := strconv.Atoi(id)
				if err != nil {
					goto restore_param
				}

				if idx >= len(params) {
					goto restore_param
				}

				if f.sanitize {
					dst = append(dst, '?')
					continue
				}
				dst = f.appendParam(dst, params[idx])
				continue
			}

			if f.namedParams != nil && !f.sanitize {
				if param, ok := f.namedParams[id]; ok {
					dst = f.appendParam(dst, param)
					continue
				}
			}

			if !namedParamsInit && len(params) > 0 {
				namedParams, ok = newTableParams(params[len(params)-1])
				if ok {
					params = params[:len(params)-1]
				}
				namedParamsInit = true
			}

			if namedParams != nil && !f.sanitize {
				dst, ok = namedParams.AppendParam(dst, id)
				if ok {
					continue
				}
			}

			if model != nil && (!f.sanitize || isStructuralParam(id)) {
				dst, ok = model.AppendParam(dst, id)
				if ok {
					continue
				}
			}

		restore_param:
			dst = append(dst, '?')
			dst = append(dst, id...)
			continue
		}

		if paramsIndex >= len(params) {
			dst = append(dst, '?')
			continue
		}

		param := params[paramsIndex]
		paramsIndex++

		if fa, ok := param.(FormatAppender); ok {
			// f carries the sanitize flag, so a nested expression or subquery is
			// rendered under the same rules rather than escaping them.
			dst = fa.AppendFormat(dst, f)
		} else if f.sanitize && !isStructuralParamValue(param) {
			dst = append(dst, '?')
		} else {
			dst = types.Append(dst, param, 1)
		}
	}

	return dst
}

func (f Formatter) appendParam(b []byte, param interface{}) []byte {
	if fa, ok := param.(FormatAppender); ok {
		return fa.AppendFormat(b, f)
	}
	return types.Append(b, param, 1)
}

// isStructuralParam reports whether a named placeholder resolves to part of the
// statement's STRUCTURE rather than to data.
//
// Table.AppendParam resolves a name against the model's fields and methods, so
// ?SomeField and ?SomeMethod both yield row data. Only the alias is structure,
// and only it may render while sanitizing; everything else falls through to be
// restored as the placeholder it was written as.
func isStructuralParam(name string) bool {
	switch name {
	case "TableName", "TableAlias":
		return true
	}
	return false
}

// isStructuralParamValue reports whether a positional parameter is part of the
// statement's structure rather than a bound value.
//
// types.F is an identifier — always structure, always safe to render.
//
// types.Q is deliberately NOT treated as structure despite being "raw SQL".
// go-pg builds relation joins by pre-rendering the parent rows' primary keys
// and wrapping them in types.Q (see the `(?) IN (?)` in join.go), so trusting
// the type would export exactly the row ids this is meant to withhold. The one
// exception is a sort direction, which is a closed two-value set and is what
// keeps ORDER BY readable.
func isStructuralParamValue(param interface{}) bool {
	switch p := param.(type) {
	case types.F:
		return true
	case types.Q:
		return isSortDirection(string(p))
	}
	return false
}

func isSortDirection(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ASC", "DESC":
		return true
	}
	return false
}

// sanitizer is implemented by the formatters that carry the sanitize flag, so
// an appender that bypasses FormatQuery entirely can still honour it.
type sanitizer interface {
	sanitizing() bool
}

func (f Formatter) sanitizing() bool { return f.sanitize }

// isSanitizing reports whether this render must withhold bound values.
func isSanitizing(f QueryFormatter) bool {
	s, ok := f.(sanitizer)
	return ok && s.sanitizing()
}
