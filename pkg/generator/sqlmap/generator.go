package sqlmap

import (
	"fmt"
	"slices"
	"strings"

	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generator/sqlmap/types"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer/filter"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer/query"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer/repo"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer/scanner"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/writer/schema"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

// plugin is one generated artifact. Order matters: writers append to the same
// file, and query's output calls the Result type scanner declares.
type plugin struct {
	Name string
	New  func(writer.Printer, types.TableRepo) writer.Writer
	// Requires names plugins whose output this one refers to.
	Requires []string
	// OptIn plugins only run when plugins= names them, as their output
	// imports a module the generated code didn't need before.
	OptIn bool
}

var plugins = []plugin{
	{Name: "schema", New: schema.New},
	{Name: "scanner", New: scanner.New},
	{Name: "query", New: query.New, Requires: []string{"scanner"}},
	// Imports github.com/snaerverk/filtrify.
	{Name: "filter", New: filter.New, OptIn: true},
	// Repository adapters bundling scanner+query+filter behind one XRepo type.
	// Imports filtrify and the sqlmap query runtime, so opt-in.
	{Name: "repo", New: repo.New, Requires: []string{"query", "filter"}, OptIn: true},
}

type SqlGenerator struct {
	plugin  *protogen.Plugin
	writers []func(writer.Printer, types.TableRepo) writer.Writer
	repo    types.TableRepo
}

func New(opts protogen.Options, request *pluginpb.CodeGeneratorRequest) (*SqlGenerator, error) {
	enabled, err := parseEnabled(request.GetParameter())
	if err != nil {
		return nil, err
	}
	plug, err := opts.New(request)
	if err != nil {
		return nil, err
	}
	// Without this protoc/buf refuse to hand over files using proto3
	// `optional`. Nothing else is needed: its synthetic oneofs carry no
	// `subtypes` option, and IsNullable already follows HasPresence.
	plug.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
	gen := &SqlGenerator{
		plugin: plug,
		repo:   make(types.TableRepo, 0),
	}
	for _, p := range plugins {
		if !enabled[p.Name] {
			continue
		}
		for _, req := range p.Requires {
			if !enabled[req] {
				return nil, fmt.Errorf("sqlmap: plugin %q needs %q, which is not enabled", p.Name, req)
			}
		}
		gen.writers = append(gen.writers, p.New)
	}
	return gen, nil
}

// parseEnabled reads `plugins=`, a `+`-separated list (protoc already uses
// `,`). All plugins but the opt-in ones are enabled when it is absent.
func parseEnabled(param string) (map[string]bool, error) {
	enabled := make(map[string]bool, len(plugins))
	var spec string
	for _, part := range strings.Split(param, ",") {
		if name, value, ok := strings.Cut(part, "="); ok && strings.TrimSpace(name) == "plugins" {
			spec = value
		}
	}
	if spec == "" {
		for _, p := range plugins {
			enabled[p.Name] = !p.OptIn
		}
		return enabled, nil
	}
	known := make([]string, len(plugins))
	for i, p := range plugins {
		known[i] = p.Name
	}
	for _, name := range strings.Split(spec, "+") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("sqlmap: unknown plugin %q in plugins=, want any of %s", name, strings.Join(known, "+"))
		}
		enabled[name] = true
	}
	return enabled, nil
}

func (p *SqlGenerator) Generate() (*pluginpb.CodeGeneratorResponse, error) {
	for _, protoFile := range p.plugin.Files {
		for _, msg := range protoFile.Messages {
			table, err := types.NewTableFromDescriptor(protoFile, msg)
			if err == nil {
				p.repo = append(p.repo, table)
			}
		}
	}
	for _, protoFile := range p.plugin.Files {
		// Well-known types are in the request too, and a bare `package` stub
		// for each breaks the build: with paths=source_relative,
		// descriptor.proto and timestamp.proto share a directory.
		if len(p.repo.ForFile(protoFile)) == 0 {
			continue
		}
		fileName := protoFile.GeneratedFilenamePrefix + ".sqlmap.go"
		g := p.plugin.NewGeneratedFile(fileName, ".")

		g.P("package ", protoFile.GoPackageName)
		for _, w := range p.writers {
			if err := w(g, p.repo).Write(protoFile); err != nil {
				return nil, fmt.Errorf("%s: %w", protoFile.Desc.Path(), err)
			}
		}
	}

	return p.plugin.Response(), nil
}
