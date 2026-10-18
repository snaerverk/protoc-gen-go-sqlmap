# protoc-gen-go-sqlmap

[![test](https://github.com/snaerverk/protoc-gen-go-sqlmap/actions/workflows/test.yaml/badge.svg)](https://github.com/snaerverk/protoc-gen-go-sqlmap/actions/workflows/test.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/snaerverk/protoc-gen-go-sqlmap.svg)](https://pkg.go.dev/github.com/snaerverk/protoc-gen-go-sqlmap)

`protoc`/`buf` plugin that turns `.proto` messages annotated with `sqlmap` extensions (table/column metadata, primary keys, foreign keys) into a `<file>.sqlmap.go` per proto file, containing:

- **`schema`** — `schema.v1.SchemaTable`/`SchemaColumn` values describing every table, consumed by [`pkg/migration`](pkg/migration) to create tables (via [ariga/atlas](https://atlasgo.io)) or generate migration files.
- **`scanner`** — a `<Message>Result` struct per table with `Scan`, `GetColValue`, and column-list helpers, for reading rows into Go without reflection.
- **`query`** — FieldMask-aware `Load<Message>`/`Load<Message>Rows` functions that select only requested columns and eagerly load related messages (one batched `IN (...)` per relation, stitched in Go), consumed by [`pkg/query`](pkg/query).

- **`filter`** (opt-in) — a `<Message>Filter` per table: a [filtrify](https://github.com/snaerverk/filtrify) `sql.Schema` mapping proto field names to qualified columns, has-many/belongs-to fields to `has()` relations, and declaring the sort columns and key for keyset pages. Turns client filters like `contains(name, "acme") AND has(books, pages > 300)` into a `query.Cond`.

These are independently switchable via the `plugins=` parameter (see below).

## Install

```bash
go install github.com/snaerverk/protoc-gen-go-sqlmap/cmd/protoc-gen-go-sqlmap@latest
```

### protoc

```bash
protoc --go-sqlmap_out=paths=source_relative:. your.proto
```

Optional `plugins=` parameter (comma-separated, protoc-style) selects which of `schema`, `scanner`, `query` to emit, `+`-separated:

```bash
protoc --go-sqlmap_out=plugins=schema+scanner+query,paths=source_relative:. your.proto
```

Omit `plugins=` to enable `schema`, `scanner` and `query`. `filter` is opt-in (`plugins=schema+scanner+query+filter`), since its output imports `github.com/snaerverk/filtrify`, which your module then needs. `query` requires `scanner` to also be enabled, since its output calls the scanner's `Result` type. The schema writer emits a `SchemaType` entry for every dialect (mysql/postgres/sqlite3) regardless — the actual dialect is chosen at runtime by `migration.New`, not at generation time.

### buf

Add the plugin to `buf.gen.yaml`, either from BSR (no local install needed):

```yaml
plugins:
  - remote: buf.build/snaerverk/go-sqlmap:v0.1.0
    out: pkg/generated/
    opt: [paths=source_relative, plugins=schema+scanner+query]
```

or as a local binary after `go install`:

```yaml
plugins:
  - local: protoc-gen-go-sqlmap
    out: pkg/generated/
    opt: [paths=source_relative]
```

## Annotating messages

Import `sqlmap/v1/sqlmap.proto` and annotate tables and columns:

```proto
syntax = "proto3";

package example;

import "sqlmap/v1/sqlmap.proto";

option go_package = "example/examplepb";

message Author {
  option (sqlmap.v1.table) = { name: "tbl_author" };
  int64 id = 1 [(sqlmap.v1.col) = { fieldname: "author_id", pk: PK_AUTO }];
  string name = 2 [(sqlmap.v1.col) = { fieldname: "author_name" }];

  // has-many: the key lives on tbl_book (author_id), not here.
  repeated Book books = 3 [(sqlmap.v1.col) = {
    foreign_key: { entity: "Book", fieldnames: ["author_id"] }
  }];
}

message Book {
  option (sqlmap.v1.table) = {
    name: "tbl_book"
    foreign_keys: [{
      fieldnames: ["author_id"]
      to: { entity: "Author", fieldnames: ["author_id"] }
      on_delete: ON_DELETE_CASCADE
    }]
  };
  int64 id = 1 [(sqlmap.v1.col) = { fieldname: "book_id", pk: PK_AUTO }];
  string title = 2 [(sqlmap.v1.col) = { fieldname: "book_title" }];
  // proto3 optional gives this field presence, so it's nullable; a bare
  // scalar has no presence and would generate NOT NULL instead.
  optional int64 author_id = 3 [(sqlmap.v1.col) = { fieldname: "author_id" }];
}
```

- `pk: PK_AUTO` for a database-generated key, `PK_MAN` for one the application supplies.
- A `message`-kind field with `foreign_key` is a relation: `repeated` is has-many (relation only, no column); a singular field is belongs-to (both a column holding the key and a relation).
- Nullability: an explicit `nullable` on the column wins; otherwise PKs are `NOT NULL` and every other column follows the proto field's presence — a `proto3 optional` or message-kind field has presence and is nullable, a bare proto3 scalar does not and becomes `NOT NULL`.
- A `message`-kind field *without* a `foreign_key` is stored as an embedded `JSON` column.

See [`proto/sqlmap/v1/sqlmap.proto`](proto/sqlmap/v1/sqlmap.proto) for the full extension surface (`ForeignKeyDefinition`, per-dialect `type` overrides) and [`docs/design/DESIGN-SUBTYPE-TABLES.md`](docs/design/DESIGN-SUBTYPE-TABLES.md) for joined-table subtype hierarchies (`oneof` + `(sqlmap.v1.subtypes)`).

## Using the generated code

```go
db, _ := sql.Open("postgres", dsn)

// Create tables from the generated schema (or use m.Diff/ApplyPending for
// migration files instead).
m, err := migration.New(db, migration.DialectPostgres)
if err != nil {
    return err
}
if err := m.Create(ctx, examplepb.AuthorTable, examplepb.BookTable); err != nil {
    return err
}

// Load authors with their books' titles eagerly loaded, selecting only the
// columns the FieldMask asks for.
conn := query.Conn{DB: db, Dialect: query.Postgres}
authors, err := examplepb.LoadAuthor(ctx, conn,
    &fieldmaskpb.FieldMask{Paths: []string{"name", "books.title"}})
```

`LoadAuthor` also takes `...query.Cond` filters (build one with `query.In(col, values)`, which reports whether the value slice was non-empty).

With the `filter` plugin, a client's filtrify expression becomes such a condition. Only fields the generated schema declares are accepted, and values are always bind parameters:

```go
expr, err := filter.Parse(ctx, `contains(name, "ann") AND has(books, pages >= 300)`)
cond, err := examplepb.AuthorFilter.Cond(expr) // filter.ErrInvalid for unknown fields etc.
authors, err := examplepb.LoadAuthor(ctx, conn, mask, query.Cond(cond))
```

`AuthorFilter.Page(order, paging, "name")` resolves a keyset page (ORDER BY, the condition after a cursor, cursors for the rows). The generated schema leaves out what filtrify can't compare portably: JSON columns, embedded messages, floats, enums, and self-referencing relations; the generated comment lists each with its reason. Only NOT NULL columns are sortable, and a composite primary key leaves the schema without a page key. For row-level access without eager loading, use `<Message>Result.Scan` directly against `GetAuthorColumns()`/`GetAuthorPKColumns()`.

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

## Docs

- [pkg.go.dev](https://pkg.go.dev/github.com/snaerverk/protoc-gen-go-sqlmap)
- [`docs/design/DESIGN-SUBTYPE-TABLES.md`](docs/design/DESIGN-SUBTYPE-TABLES.md) — joined-table subtype hierarchies.
- [`docs/design/DESIGN-FILTRIFY-CONDITIONS.md`](docs/design/DESIGN-FILTRIFY-CONDITIONS.md) — planned: translating a [filtrify](https://github.com/snaerverk/filtrify) AST into SQL filters at runtime.

## Roadmap

- [x] Schema generation + atlas-backed migrations (create tables, diff, migration files)
- [x] FieldMask-aware queries with eager loading (has-many and belongs-to, arbitrary depth)
- [x] Joined-table subtypes (`oneof` enforced as a DB-level FK/CHECK constraint)
- [x] Multiple dialects: PostgreSQL, MySQL/MariaDB, SQLite
- [x] Published as a BSR remote plugin (`buf.build/snaerverk/go-sqlmap`)
- [x] SQL filters from a [filtrify](https://github.com/snaerverk/filtrify) AST, via the generated `filter` schemas
- [ ] Paged queries (search + order + cursor + count) as one generated call
- [ ] Insert/update helpers (currently out of scope; `pkg/migration` only creates schema, `pkg/query` only reads)
