// Package filter emits a filtrify SQL schema per table: the filter-facing view
// of an entity. Filters name proto fields, never columns, so the schema maps
// each filterable field to its qualified column and filter kind, has-many and
// belongs-to fields to correlated EXISTS relations, and declares the columns
// a page may be ordered by.
//
// Only the generated code imports filtrify; this module does not depend on it.
package filter

import (
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generator/sqlmap/types"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	filterImportPath = protogen.GoImportPath("github.com/snaerverk/filtrify/pkg/filter")
	sqlImportPath    = protogen.GoImportPath("github.com/snaerverk/filtrify/pkg/filter/sql")
)

const templateStr = `
{{- $f := .FilterPkg }}{{ $s := .SQLPkg }}
// {{ .Name }}Filter declares what filtrify filters and page orders over
// {{ .TableName }} may name: proto field names, mapped to qualified columns.
{{- if .Skipped }}
// Not filterable:
{{- range .Skipped }}
//   - {{ .Name }}: {{ .Reason }}
{{- end }}
{{- end }}
var {{ .Name }}Filter = &{{ $s }}.Schema{
	Columns: map[string]{{ $s }}.Column{
		{{- range .Columns }}
		{{ q .Name }}: {SQL: {{ q .SQL }}, Kind: {{ $f }}.{{ .Kind }}},
		{{- end }}
	},
	Sortable: []string{ {{- range $i, $c := .Sortable }}{{ if $i }}, {{ end }}{{ q $c }}{{ end -}} },
	{{- with .Key }}
	Key:      {{ $s }}.Column{SQL: {{ q .SQL }}, Kind: {{ $f }}.{{ .Kind }}},
	{{- end }}
}
{{- if .Relations }}

// Relations refer to other tables' schemas, so they are set once all exist.
func init() {
	{{ .Name }}Filter.Relations = map[string]{{ $s }}.Relation{
		{{- range .Relations }}
		{{ q .Name }}: {From: {{ q .From }}, Join: {{ q .Join }}, Schema: {{ .Schema }}},
		{{- end }}
	}
}
{{- end }}
`

var tpl = template.Must(template.New("filter").Funcs(template.FuncMap{"q": strconv.Quote}).Parse(templateStr))

type Column struct {
	Name string
	SQL  string
	Kind string
}

type Relation struct {
	Name   string
	From   string
	Join   string
	Schema string
}

type Skipped struct {
	Name   string
	Reason string
}

type Table struct {
	Name      string
	TableName string
	FilterPkg string
	SQLPkg    string
	Columns   []*Column
	Sortable  []string
	Key       *Column
	Relations []*Relation
	Skipped   []*Skipped
}

type FilterWriter struct {
	o    writer.Printer
	repo types.TableRepo
}

func New(g writer.Printer, repo types.TableRepo) writer.Writer {
	return &FilterWriter{o: g, repo: repo}
}

func (w *FilterWriter) Write(protoFile *protogen.File) error {
	for _, table := range w.repo.ForFile(protoFile) {
		if err := w.table(table); err != nil {
			return err
		}
	}
	return nil
}

func (w *FilterWriter) table(table *types.Table) error {
	tbl := Table{
		Name:      table.GetMessageName(),
		TableName: table.GetTableName(),
		FilterPkg: w.pkgAlias("Kind", filterImportPath),
		SQLPkg:    w.pkgAlias("Schema", sqlImportPath),
	}

	var pks []*Column
	for _, c := range table.GetColumns() {
		kind, reason, err := Kind(w.repo, c)
		if err != nil {
			return fmt.Errorf("table %q column %q: %w", table.GetTableName(), c.GetFieldname(), err)
		}
		if kind == "" {
			tbl.Skipped = append(tbl.Skipped, &Skipped{Name: c.GetProtoName(), Reason: reason})
			continue
		}
		col := &Column{
			Name: c.GetProtoName(),
			SQL:  table.GetTableName() + "." + c.GetFieldname(),
			Kind: kind,
		}
		tbl.Columns = append(tbl.Columns, col)
		// Keyset pagination needs a total order, and databases disagree on
		// where NULLs sort.
		if !c.IsNullable() {
			tbl.Sortable = append(tbl.Sortable, col.Name)
		}
		if c.IsPrimaryKey() {
			pks = append(pks, col)
		}
	}
	// Pages need a single-column key as tiebreaker; a composite one leaves
	// Key unset, which filtrify refuses to paginate.
	if len(pks) == 1 {
		tbl.Key = pks[0]
	}

	for _, rel := range table.GetRelations() {
		out, reason, err := w.relation(table, rel)
		if err != nil {
			return err
		}
		if out == nil {
			tbl.Skipped = append(tbl.Skipped, &Skipped{Name: "has(" + rel.GetProtoName() + ", ...)", Reason: reason})
			continue
		}
		tbl.Relations = append(tbl.Relations, out)
	}

	return tpl.Execute(w.o, tbl)
}

// relation maps a has-many or belongs-to field to an EXISTS over the target
// table: target.key = table.key, with the target's own schema inside.
func (w *FilterWriter) relation(table *types.Table, rel *types.Relation) (*Relation, string, error) {
	target, err := rel.GetTarget(w.repo)
	if err != nil {
		return nil, "", err
	}
	// Both sides would carry the same table name, and the subquery's
	// columns would shadow the parent's.
	if target.GetTableName() == table.GetTableName() {
		return nil, "self-referencing relations need table aliases, not supported yet", nil
	}
	remote, err := rel.GetTargetColumns(w.repo)
	if err != nil {
		return nil, "", err
	}
	local, err := rel.GetLocalColumns(w.repo)
	if err != nil {
		return nil, "", err
	}
	if len(remote) != 1 || len(local) != 1 {
		return nil, "", fmt.Errorf("table %q relation %q: composite keys are not supported yet (%d local, %d remote columns)",
			table.GetTableName(), rel.GetName(), len(local), len(remote))
	}

	schema := target.GetMessageName() + "Filter"
	if target.File.GoImportPath != table.File.GoImportPath {
		schema = w.o.QualifiedGoIdent(protogen.GoIdent{GoName: schema, GoImportPath: target.File.GoImportPath})
	}
	return &Relation{
		Name:   rel.GetProtoName(),
		From:   target.GetTableName(),
		Join:   target.GetTableName() + "." + remote[0].GetFieldname() + " = " + table.GetTableName() + "." + local[0].GetFieldname(),
		Schema: schema,
	}, "", nil
}

// Kind returns the filtrify kind (the name of its filter.Kind constant) of a
// column, or "" and the reason it can't be filtered.
func Kind(repo types.TableRepo, c *types.Column) (kind, reason string, err error) {
	// A JSON column compares as JSON, differently in every dialect, even if
	// the proto field is a plain string holding it.
	for _, t := range c.Def.GetType() {
		if strings.Contains(strings.ToUpper(t), "JSON") {
			return "", "JSON column", nil
		}
	}
	switch c.Field.Desc.Kind() {
	case protoreflect.StringKind:
		return "String", "", nil
	case protoreflect.BoolKind:
		return "Bool", "", nil
	case protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		return "Int", "", nil
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "", "filtrify has no float kind yet", nil
	case protoreflect.EnumKind:
		return "", "enums are not supported yet", nil
	case protoreflect.MessageKind:
		if c.IsTimestamp() {
			return "Time", "", nil
		}
		if c.Def.ForeignKey == nil {
			return "", "embedded message (JSON)", nil
		}
		// A belongs-to column holds the target's key: filter on that value.
		target, ok := repo.GetByName(c.GetForeignKeyEntity())
		if !ok {
			return "", "", fmt.Errorf("foreign key table %q not found", c.GetForeignKeyEntity())
		}
		refs, err := types.ResolveRefColumns(target, c.Def.ForeignKey.GetFieldnames())
		if err != nil {
			return "", "", err
		}
		return Kind(repo, refs[0])
	default:
		return "", "unsupported type", nil
	}
}

// pkgAlias resolves the local alias protogen assigned to an import.
func (w *FilterWriter) pkgAlias(goName string, path protogen.GoImportPath) string {
	return strings.TrimSuffix(w.o.QualifiedGoIdent(protogen.GoIdent{GoName: goName, GoImportPath: path}), "."+goName)
}
