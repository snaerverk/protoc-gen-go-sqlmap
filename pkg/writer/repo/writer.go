// Package repo emits a generic repository adapter per table: a zero-size
// XRepo type bundling the scanner, query, and filter outputs behind one set of
// methods, so a server can paginate an entity without hand-written glue.
//
// Only the generated code imports filtrify and the sqlmap query runtime; this
// module does not depend on them.
package repo

import (
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generator/sqlmap/types"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer/filter"
	"google.golang.org/protobuf/compiler/protogen"
)

const (
	queryImportPath       = protogen.GoImportPath("github.com/snaerverk/protoc-gen-go-sqlmap/pkg/query")
	filterImportPath      = protogen.GoImportPath("github.com/snaerverk/filtrify/pkg/filter")
	sqlImportPath         = protogen.GoImportPath("github.com/snaerverk/filtrify/pkg/filter/sql")
	contextImportPath     = protogen.GoImportPath("context")
	fieldmaskpbImportPath = protogen.GoImportPath("google.golang.org/protobuf/types/known/fieldmaskpb")
)

const templateStr = `
{{- $ctx := .CtxPkg }}{{ $q := .QueryPkg }}{{ $s := .SQLPkg }}{{ $f := .FilterPkg }}{{ $fm := .FieldmaskPkg }}
// {{ .Name }}Repo is a generated repository adapter for {{ .TableName }}: it
// implements a server's generic read/paginate interface over the scanner,
// query, and filter outputs for this table.
type {{ .Name }}Repo struct{}

func ({{ .Name }}Repo) Load(ctx {{ $ctx }}.Context, c {{ $q }}.Conn, mask *{{ $fm }}.FieldMask, conds ...{{ $q }}.Cond) ([]*{{ .Name }}, error) {
	return Load{{ .Name }}(ctx, c, mask, conds...)
}

func ({{ .Name }}Repo) Filter() *{{ $s }}.Schema { return {{ .Name }}Filter }

func ({{ .Name }}Repo) Name() string { return {{ q .TableName }} }

func ({{ .Name }}Repo) PKColumn() string { return {{ q .PKColumn }} }

func ({{ .Name }}Repo) Columns(m *{{ $q }}.Mask) []string { return {{ .Name }}QueryColumns(m) }

func ({{ .Name }}Repo) ScanRow(cols []string, s {{ $q }}.Scanner) (*{{ .Name }}, error) {
	res := new({{ .Name }}Result)
	if err := res.Scan(cols, s); err != nil {
		return nil, err
	}
	return &res.{{ .Name }}, nil
}

func ({{ .Name }}Repo) SortFields() map[string]{{ $f }}.Field[*{{ .Name }}] {
	return map[string]{{ $f }}.Field[*{{ .Name }}]{
		{{- range .Sort }}
		{{ q .Name }}: {Kind: {{ $f }}.{{ .Kind }}, Get: func(x *{{ $.Name }}) []any { return []any{ {{ .Box }} } }},
		{{- end }}
	}
}
`

var tpl = template.Must(template.New("repo").Funcs(template.FuncMap{"q": strconv.Quote}).Parse(templateStr))

type sortField struct {
	Name string // proto field name (matches the filter schema's Sortable)
	Kind string // filtrify filter.Kind constant name
	Box  string // expression yielding the value, e.g. "x.GetId()"
}

type table struct {
	Name         string
	TableName    string
	PKColumn     string
	CtxPkg       string
	QueryPkg     string
	SQLPkg       string
	FilterPkg    string
	FieldmaskPkg string
	Sort         []*sortField
}

type RepoWriter struct {
	o    writer.Printer
	repo types.TableRepo
}

func New(g writer.Printer, repo types.TableRepo) writer.Writer {
	return &RepoWriter{o: g, repo: repo}
}

func (w *RepoWriter) Write(protoFile *protogen.File) error {
	for _, t := range w.repo.ForFile(protoFile) {
		if err := w.table(t); err != nil {
			return err
		}
	}
	return nil
}

func (w *RepoWriter) table(t *types.Table) error {
	// A single-column primary key is required: it is the keyset tiebreaker and
	// the lookup-by-id column. Keyless or composite-key tables get no repo.
	var pk *types.Column
	for _, c := range t.GetColumns() {
		if c.IsPrimaryKey() {
			if pk != nil {
				return nil // composite key: skip
			}
			pk = c
		}
	}
	if pk == nil {
		return nil
	}

	tbl := table{
		Name:         t.GetMessageName(),
		TableName:    t.GetTableName(),
		PKColumn:     t.GetTableName() + "." + pk.GetFieldname(),
		CtxPkg:       w.pkgAlias("Context", contextImportPath),
		QueryPkg:     w.pkgAlias("Conn", queryImportPath),
		SQLPkg:       w.pkgAlias("Schema", sqlImportPath),
		FilterPkg:    w.pkgAlias("Field", filterImportPath),
		FieldmaskPkg: w.pkgAlias("FieldMask", fieldmaskpbImportPath),
	}

	for _, c := range t.GetColumns() {
		kind, _, err := filter.Kind(w.repo, c)
		if err != nil {
			return fmt.Errorf("table %q column %q: %w", t.GetTableName(), c.GetFieldname(), err)
		}
		// Keyset pagination needs a total order; nullable columns are left out
		// of Sortable (and so of SortFields), matching the filter schema.
		if kind == "" || c.IsNullable() {
			continue
		}
		box, err := boxExpr(kind, "x.Get"+c.Field.GoName+"()")
		if err != nil {
			return fmt.Errorf("table %q column %q: %w", t.GetTableName(), c.GetFieldname(), err)
		}
		tbl.Sort = append(tbl.Sort, &sortField{Name: c.GetProtoName(), Kind: kind, Box: box})
	}

	return tpl.Execute(w.o, tbl)
}

// boxExpr adapts a proto getter's value to the type filtrify's cursor expects
// for the kind (int64, time.Time, string, bool).
func boxExpr(kind, getter string) (string, error) {
	switch kind {
	case "String", "Bool":
		return getter, nil
	case "Int":
		return "int64(" + getter + ")", nil
	case "Time":
		return getter + ".AsTime()", nil
	default:
		return "", fmt.Errorf("unsupported sort kind %q", kind)
	}
}

// pkgAlias resolves the local alias protogen assigned to an import.
func (w *RepoWriter) pkgAlias(goName string, path protogen.GoImportPath) string {
	return strings.TrimSuffix(w.o.QualifiedGoIdent(protogen.GoIdent{GoName: goName, GoImportPath: path}), "."+goName)
}
