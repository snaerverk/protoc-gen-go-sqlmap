# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`protoc-gen-go-sqlmap` is a `protoc`/`buf` code-generator plugin. Given `.proto` messages annotated with `sqlmap` extensions (table/column metadata, primary keys, foreign keys), it emits a `<file>.sqlmap.go` per proto file containing `schema.v1.SchemaTable`/`SchemaColumn` values plus row-scanning helpers. `pkg/migration` then converts those values into `ariga/atlas` schema types to create tables or write migration files.

The goal is a set of independently switchable plugins over one shared `TableRepo`: (1) atlas migrations — **done**; (2) queries with eager loading of children + FieldMask support — **done**; (3) SQL filters built from the filtrify AST (`github.com/snaerverk/filtrify`) — **done** as the opt-in `filter` plugin: it generates a filtrify `sql.Schema` per table and filtrify's `pkg/filter/sql` does the translation; (4) repository adapters — **done** as the opt-in `repo` plugin: it emits one `<Msg>Repo` type per single-PK table, bundling the scanner, query, and filter outputs behind `Load`/`Filter`/`PKColumn`/`Columns`/`ScanRow`/`SortFields` so a consumer can drive generic lookup and keyset pagination without hand-written glue. Note `docs/plans/sqlmap-generator/PLAN-SQLMAP-GENERATOR.md` is stale on (3): its Phase 5 sketches ad-hoc per-column builder funcs rather than translating an external AST.

Plugins are toggled with the `plugins=` protoc parameter, a **`+`-separated** list (protoc already uses `,` between parameters): `--go-sqlmap_out=plugins=schema+scanner:.`. Omitting it enables everything. `plugins` entries live in one slice in `generator.go`; `query` declares `Requires: ["scanner"]` because its output calls the scanner's `Result` type, and enabling it alone is a generation-time error rather than uncompilable Go. `filter` is `OptIn`: omitting `plugins=` doesn't enable it, because its output imports filtrify and existing consumers must not suddenly need that module. `repo` is opt-in for the same reason (it imports filtrify and the query runtime) and declares `Requires: ["query", "filter"]`, since its output calls `Load<Msg>`, `<Msg>QueryColumns`, the `Result` type, and `<Msg>Filter`.

### BSR remote plugin

`buf.plugin.yaml` + `Dockerfile` publish the generator as `buf.build/snaerverk/go-sqlmap`, so consumers need no local binary:

```yaml
plugins:
  - remote: buf.build/snaerverk/go-sqlmap:v0.1.0
    out: pkg/generated/
    opt: [paths=source_relative, plugins=schema+scanner+query]
```

`buf.plugin.yaml` pins the runtime module the generated code imports (`pkg/query`, `pkg/generated/schema/v1`), so `plugin_version` and `registry.go.deps` must both match the git tag — the release workflow fails if they drift. Publishing happens in CI on tag (`buf beta registry plugin push`, needs `BUF_TOKEN`); it is a separate resource from the `buf.build/snaerverk/protoc-gen-go-sqlmap` *module* that carries the protos. Test the image locally with `docker build -t x . && protoc --plugin=protoc-gen-y=<script running 'docker run --rm -i x'>`.

**Dependency rule: only `ariga.io/atlas` and `google.golang.org/protobuf` are allowed as direct dependencies.** Ask before adding any other, test-only ones included — prefer plain stdlib loops over utility libraries. Target Go 1.27.

## Commands

```bash
task install         # go mod tidy && go install ./cmd/protoc-gen-go-sqlmap/
make install          # buf generate && go install (regenerates pkg/generated/** from proto/)
buf generate          # regenerate pkg/generated/**.pb.go from proto/
go build ./...
go test ./...                                            # golden + unit tests
go test ./pkg/generator/sqlmap/ -update                  # accept new golden output
SQLMAP_E2E=1 go test ./pkg/generator/sqlmap/ -run TestE2E # dockerized-Postgres end-to-end
```

CI (`.github/workflows/test.yaml`) installs protoc + `protoc-gen-go` + buf, runs `make install`, then `go test -run=.`.

### Tests

- `pkg/generator/sqlmap/golden_test.go` — runs `testdata/*.proto` through protoc + the generator in-process and diffs against `testdata/*.sqlmap.go.golden`. `-update` rewrites them.
- `pkg/migration/migration_test.go` — unit tests over `toSchema` (FK pointer identity, auto-increment per dialect, nullability, SET-NULL validation).
- `pkg/query/query_test.go` — mask parsing (including that a broad path beats a narrow one in either order), placeholder renumbering, join-key normalization.
- `pkg/generator/sqlmap/plugins_test.go` — `plugins=` parsing, the scanner dependency, and that a disabled plugin's output really disappears.
- `pkg/generator/sqlmap/subtype_test.go` — misconfigured hierarchies (`testdata/subtype_bad_*.proto`) must fail generation naming the offending declaration. `generateError` fails the test if generation *succeeds*.
- `pkg/generator/sqlmap/e2e_test.go` — opt-in (`SQLMAP_E2E=1`, needs docker). `runE2E` starts a throwaway PostgreSQL container, generates a testdata proto through the real pipeline into a temp Go module, and runs a driver against the live database. Cases: `relation.proto` (nullability, `ON DELETE SET NULL`), `eager.proto` (FieldMask column selection, eager loading both directions, two levels deep), `subtype.proto`, and `filter.proto` (generated filter schemas through `Load<Msg>`, `has()` both directions and nested, keyset pages with ties). `runE2EWith` takes a `plugins=` value and extra requirements for the temp module — filtrify for the filter case, pinned in `filtrifyModule`. They need separate containers because both protos declare `tbl_author`/`tbl_book`. The SQL driver (`lib/pq`) lives in the temp module's `go.mod`, never in this repo's — that's how the e2e keeps the two-dependency rule.

## Architecture

### Generation pipeline

```
.proto file (sqlmap extensions)
    │
    ▼
cmd/protoc-gen-go-sqlmap/main.go       reads CodeGeneratorRequest from stdin, parses `dialect=` param (default "postgres")
    │
    ▼
pkg/generator/sqlmap/generator.go       SqlGenerator.Generate():
    │                                     1. builds a TableRepo by scanning every message for the (sqlgen.table) option
    │                                     2. for each proto file, runs every registered writer against that repo
    ▼
pkg/writer/schema/writer.go              emits schemav1.SchemaTable + SchemaColumn vars, FK wiring via init()
pkg/writer/scanner/writer.go             emits `<Msg>Result` struct with Scan(), GetColValue(), Get<Msg>Columns()/PKColumns()
pkg/writer/query/writer.go               emits <Msg>QueryColumns(), Load<Msg>Rows(), Load<Msg>()
pkg/writer/filter/writer.go              (opt-in) emits <Msg>Filter, a filtrify sql.Schema; relations wired via init()
    │
    ▼
<file>.sqlmap.go   (written via protogen.GeneratedFile, package = proto file's go_package)
    │
    ▼
pkg/migration (runtime, not generated)   toSchema() → atlas schema.Table; Create/Diff/ApplyPending
pkg/query     (runtime, not generated)   Mask, Conn/Select, In/Key/Keys placeholder + join-key helpers
```

Writers are plain functions registered in a slice in `generator.go` (`writers: []func(writer.Printer, types.TableRepo) writer.Writer{schema.New, scanner.New}`). Adding a new generated artifact means adding a new writer package + appending it to that slice — no changes needed to the core loop.

Writers use `text/template` against small local `Table`/`Column` view-model structs — they translate the richer `types.Table`/`types.Column` domain model into template-friendly shapes right before executing. (They used `html/template` until it was found to escape quotes inside substituted values, which mangles emitted SQL such as `kind IN ('person')`; only template *literals* are left alone, so the bug stayed hidden until a value contained a quote.)

### Domain model (`pkg/generator/sqlmap/types`)

- `TableRepo` (`[]*Table`) — all tables across all processed proto files; `GetByName` looks up by message name, used to resolve cross-message foreign keys (including across proto files/packages).
- `Table` wraps a `*protogen.Message` + its `sqlmapv1.Table` extension; `NewTableFromDescriptor` returns an error (not a panic) for messages that don't carry the `(sqlgen.table)` option, and the generator silently skips those — this is the mechanism that decides which messages become tables.
- `Column` wraps a `*protogen.Field` + its `sqlmapv1.Column` extension the same way; fields without `(sqlgen.col)` are skipped.
- `Column.GetSqlType(repo, dialect)` resolves the SQL type: explicit `Def.Type[dialect]` wins; otherwise it falls back by proto kind (bool→BOOLEAN, int32→INT(11), int64→BIGINT, string→VARCHAR(255), float→FLOAT; int32/int64 →INTEGER on sqlite3, which AUTOINCREMENT requires). A `MessageKind` field with a `foreign_key` follows the reference into the target table (via `repo.GetByName`) and recurses into that table's PK (or the FK's explicit `fieldnames`) to inherit its type; a `MessageKind` field *without* a foreign key becomes an embedded `JSON` column. Resolution failures are returned as errors, never baked into the generated file.
- `Column.IsNullable()` — an explicit `nullable` in the column option wins; otherwise PKs are NOT NULL and every other column follows `Field.Desc.HasPresence()` (proto2 `optional`, proto3 `optional`, and message fields have presence; a proto3 bare scalar does not). `pkg/migration` rejects `ON DELETE SET NULL` on a NOT NULL column rather than emitting DDL the database will refuse.
- The schema writer emits a `SchemaType` entry for **every** dialect in its `dialects` var (mysql/postgres/sqlite3), so one generated file serves any of them; the dialect is chosen at runtime by `migration.New`, not at generation time.

### Filter schemas (`pkg/writer/filter`)

`<Msg>Filter` is the *filter-facing* view of a table, distinct from the migration view `SchemaTable`: keyed by proto field name, an allow-list, filter kinds instead of SQL types, relations named after the parent's field. Columns are emitted as `table.column` (relations put them inside a correlated `EXISTS`). Mapping: string→String, every integer kind→Int, bool→Bool, Timestamp→Time, a belongs-to column→its target key's kind. Skipped with a reason in the generated comment: explicit `JSON`/`JSONB` types, embedded messages, floats, enums, self-referencing relations (both sides would share a table name; needs aliases). `Sortable` is the NOT NULL columns (keyset paging can't cope with NULL ordering differences); `Key` is the single-column PK, unset for composite keys, which filtrify then refuses to paginate. Generating it adds no dependency to this module — only the generated code imports filtrify.

### Relations and eager loading (`types/relation.go`, `pkg/writer/query`)

A message-kind field with a `foreign_key` is a **relation**; cardinality picks the direction:

- **singular** — belongs-to. Both a column (holding the key) and a relation; the scanner keeps the raw key in `fk_<column>_id`.
- **repeated** — has-many. A relation *only*: `NewTableFromDescriptor` keeps it out of `GetColumns()`, which stops it becoming a bogus column and schema FK.

`ForeignKey.Fieldnames` always names columns on the *target*, resolved by `types.ResolveRefColumns` (SQL name first, then Go name).

Related rows are fetched with one batched `IN (...)` per relation and stitched in Go, not joined. A child load calls the child's own `Load<T>Rows`, so nesting is recursive and `books.publisher.name` works for free; `extra ...string` forces the join column into the child's SELECT.

The mask drives column selection *and* which relations load. PKs are always selected, since stitching keys on them. `query.Key` normalizes join keys because drivers disagree on integer width and `[]byte` vs `string`.

### Joined-table subtypes (`types/subtype.go`, `docs/design/DESIGN-SUBTYPE-TABLES.md`)

A `oneof` carrying `option (sqlmap.v1.subtypes) = {discriminator: "kind"}` makes its message a supertype; each arm's table declares `subtype_of` in its table option. The supertype gets a synthesized `kind` column, `CHECK (kind IN (...))`, and a **unique index over (pk..., kind)** — a FK can only reference a uniquely-constrained set. Each subtype gets the same column with `DEFAULT`/`CHECK` pinning it, plus a **composite** FK `(key..., kind) -> super(pk..., kind)`. That makes "at most one subtype" a database guarantee; at-least-one is not declaratively enforceable and is left to the application.

The discriminator has no proto field, so its var is `<Msg>DiscriminatorColumn`, avoiding collision with a real field. A subtype's key columns must *not* also declare a `foreign_key` — `subtype_of` implies it.

`SchemaCheck.expr` is a **per-dialect map**: without `ANSI_QUOTES`, MySQL reads `"kind"` as a *string literal*, so the CHECK is accepted and then rejects every row. `toSchema` errors rather than falling back to another dialect's expression. Fragments go through the template's `q` (`strconv.Quote`), since a MySQL expression contains backticks.

### Proto extensions (`proto/sqlmap/v1/sqlmap.proto`)

Note this is `syntax = "proto2"` — extension fields are `optional`/`required`, and the pattern for "is this set" is `Def.Field != nil` rather than zero-value checks.

- `extend google.protobuf.MessageOptions { optional Table table = 800100; }` — put on a message to make it a table. `Table{name, foreign_keys}`.
- `extend google.protobuf.FieldOptions { optional Column col = 800120; }` — put on a field to make it a column. `Column{fieldname, pk (PK_AUTO|PK_MAN), type (map[dialect]sql_type), foreign_key, nullable}`.
- `ForeignKeyDefinition{fieldnames, to: ForeignKey{entity, fieldnames}, on_delete}` — table-level FK declarations combine with column-level `foreign_key` (via `Table.GetForeignKeys()`, which merges both sources).
- **`README.md`'s usage example is stale** — it shows a `crud: [C,R,D]` field on `Table` that no longer exists in the .proto. Trust the `.proto` file and `pkg/generated/sqlmap/v1/sqlmap.pb.go` over the README when they disagree.

After editing `sqlmap.proto`, regenerate the extension Go types with `buf generate` (per `buf.gen.yaml`) before building — `pkg/generated/sqlmap/v1/sqlmap.pb.go` is generated, not hand-written.

### Writer/Printer abstraction (`pkg/writer/print.go`)

`Printer` is a minimal interface (`P`, `Write`, `QualifiedGoIdent`, `Import`) satisfied by `*protogen.GeneratedFile`, so writer packages depend on it instead of `protogen` directly — keeps `pkg/writer/schema` and `pkg/writer/scanner` testable/mockable without a real `protogen.Plugin`. `Writer` is the per-writer contract: `Write(*protogen.File) error`.

### Cross-file/package foreign keys

When a FK's referenced table lives in a different proto file (`refTable.File.GoImportPath != table.File.GoImportPath`), `schema/writer.go` calls `s.o.QualifiedGoIdent(...)` to get an import-qualified reference instead of a bare identifier — this is the mechanism that makes FKs work across proto packages.
