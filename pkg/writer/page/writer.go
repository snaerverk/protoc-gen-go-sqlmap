// Package page emits Query<Msg>: one paged query per table (search, order,
// cursor, count, mask-driven load) on top of the filter and query plugins'
// output, run by the pkg/page runtime.
package page

import (
	"strconv"
	"strings"
	"text/template"

	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generator/sqlmap/types"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer"
	"google.golang.org/protobuf/compiler/protogen"
)

const (
	pageImportPath      = protogen.GoImportPath("github.com/snaerverk/protoc-gen-go-sqlmap/pkg/page")
	queryImportPath     = protogen.GoImportPath("github.com/snaerverk/protoc-gen-go-sqlmap/pkg/query")
	sqlImportPath       = protogen.GoImportPath("github.com/snaerverk/filtrify/pkg/filter/sql")
	fieldmaskImportPath = protogen.GoImportPath("google.golang.org/protobuf/types/known/fieldmaskpb")
	contextImportPath   = protogen.GoImportPath("context")
)

const templateStr = `
{{- $p := .PagePkg }}{{ $q := .QueryPkg }}{{ $msg := .Name }}
{{- if .Reason }}
// No Query{{ $msg }}: {{ .Reason }}.
{{- else }}
// Query{{ $msg }} returns one page of {{ .TableName }} rows matching req, ordered by
// req.Order or the key, with the fields and relations mask selects (all
// columns and no relations for an empty mask), and the total across pages.
// Searches name {{ $msg }}Filter's fields.
func Query{{ $msg }}(ctx {{ .CtxPkg }}.Context, c {{ $q }}.Conn, req {{ $p }}.Request, mask *{{ .FMPkg }}.FieldMask) (*{{ $p }}.Result[*{{ $msg }}], error) {
	res, err := {{ $p }}.Query(ctx, c, {{ $p }}.Table[*{{ $msg }}Result]{
		Name:         {{ q .TableName }},
		Schema:       {{ $msg }}Filter,
		DefaultOrder: {{ .SQLPkg }}.KeyField,
		Load:         Load{{ $msg }}Rows,
		Key:          func(r *{{ $msg }}Result) any { return r.Get{{ .KeyGoName }}() },
	}, req, {{ $q }}.FromFieldMask(mask))
	if err != nil {
		return nil, err
	}
	out := &{{ $p }}.Result[*{{ $msg }}]{Total: res.Total, Page: res.Page}
	for _, row := range res.Rows {
		out.Rows = append(out.Rows, &row.{{ $msg }})
	}
	return out, nil
}
{{- end }}
`

var tpl = template.Must(template.New("page").Funcs(template.FuncMap{"q": strconv.Quote}).Parse(templateStr))

type Table struct {
	Name      string
	TableName string
	KeyGoName string
	// Reason is why no Query<Msg> is generated, if so.
	Reason   string
	PagePkg  string
	QueryPkg string
	SQLPkg   string
	FMPkg    string
	CtxPkg   string
}

type PageWriter struct {
	o    writer.Printer
	repo types.TableRepo
}

func New(g writer.Printer, repo types.TableRepo) writer.Writer {
	return &PageWriter{o: g, repo: repo}
}

func (w *PageWriter) Write(protoFile *protogen.File) error {
	for _, table := range w.repo.ForFile(protoFile) {
		tbl := Table{
			Name:      table.GetMessageName(),
			TableName: table.GetTableName(),
		}
		// Mirrors the filter writer's Key: pages need one key column, read
		// back through its getter.
		switch pks := table.GetPKColumns(); {
		case len(pks) != 1:
			tbl.Reason = "pages need a single-column primary key"
		case pks[0].IsMessage():
			tbl.Reason = "its primary key is a reference, which has no getter to read the key with"
		default:
			tbl.KeyGoName = pks[0].GetName()
			tbl.PagePkg = w.pkgAlias("Query", pageImportPath)
			tbl.QueryPkg = w.pkgAlias("Conn", queryImportPath)
			tbl.SQLPkg = w.pkgAlias("KeyField", sqlImportPath)
			tbl.FMPkg = w.pkgAlias("FieldMask", fieldmaskImportPath)
			tbl.CtxPkg = w.pkgAlias("Context", contextImportPath)
		}
		if err := tpl.Execute(w.o, tbl); err != nil {
			return err
		}
	}
	return nil
}

// pkgAlias resolves the local alias protogen assigned to an import.
func (w *PageWriter) pkgAlias(goName string, path protogen.GoImportPath) string {
	return strings.TrimSuffix(w.o.QualifiedGoIdent(protogen.GoIdent{GoName: goName, GoImportPath: path}), "."+goName)
}
