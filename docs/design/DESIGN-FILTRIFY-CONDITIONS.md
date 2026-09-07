# DESIGN: filtrify AST as SQL conditions

Status: proposal. Nothing implemented.

## Goal

Turn a `filtrify.ast.v1.Expr` into a SQL `WHERE` fragment for the generated loaders, with the builder living in one package rather than being emitted per proto file.

## This needs no code generation

`Load<Msg>` already takes conditions:

```go
func LoadAuthor(ctx, c query.Conn, mask *fieldmaskpb.FieldMask, conds ...query.Cond) ([]*Author, error)
```

A filter is just another `query.Cond`. So there is no third *writer* — the translator is a runtime function in a new `pkg/filter`, and the generated files stay as they are:

```go
cond, err := filter.Build(crmv1.PersonTable, expr)   // *astv1.Expr -> query.Cond
people, err := crmv1.LoadPerson(ctx, conn, mask, cond)
```

That is the whole point of the plugin roadmap's third item collapsing: the work is metadata plus a translator, not templates.

## Metadata the runtime is missing

Three small additions to what the schema writer already emits — none of them a new per-message artifact:

| Gap | Why | Change |
|---|---|---|
| `SchemaColumn` carries only the SQL name | filters name *proto* fields (`given_name`); filtrify never sees a column name | add `proto_name` |
| `SchemaTable` has no message name | `HasChildren.entity` names an entity | add `entity` |
| No way to look a table up at runtime | `HasChildren` must find the child table and its foreign key | emit a per-file `var Tables = []*SchemaTable{...}` |

The registry also closes an existing gap: `migration.Create` currently makes callers hand-list every table including foreign-key targets.

## Where the translator lives

This is the decision that blocks everything else.

**filtrify speaks message fields and entities, never tables and columns.** That is the constraint that settles the shape: the AST cannot resolve `given_name` to `person.given_name`, so whoever translates must hold the sqlmap metadata. SQL assembly already lives here too — `query.Cond`, `Conn.Select`, placeholder renumbering. So the translator belongs on the **sqlmap side**, and the only thing it needs from filtrify is the AST *types*.

That runs into this repo's rule of only `ariga.io/atlas` and `google.golang.org/protobuf` as direct dependencies.

1. **sqlmap imports the AST package**, with the dependency rule relaxed for it. Cheap in principle — the AST is a handful of small messages — except `filtrify/ast/v1` currently also contains `service_grpc.pb.go`, so importing it links grpc into every consumer of a generated data layer. **Recommended, if the AST messages move to a package without the service** (`ast.proto` → `astv1`, `service.proto` → its own). Then the real cost is protobuf, which sqlmap already has.
2. **A bridge module** depending on both, leaving sqlmap and filtrify untouched. Honest separation, but the translator needs the schema metadata intimately — columns, foreign keys, the registry — so the bridge would import sqlmap's internals anyway, and every metadata change (adding `proto_name`, say) becomes a two-module version dance.
3. **Translator in filtrify, names supplied by a caller interface.** Keeps filtrify ignorant of column names in the letter, but puts SQL generation — quoting, the `EXISTS` shape, dialect differences — in the package that is meant to be storage-agnostic, and duplicates the placeholder logic already in `pkg/query`.

Rejected: walking the AST through `protoreflect` to avoid the dependency entirely. It is possible, since the AST is a proto message, but it means matching field names as strings with no compile-time check, in the one piece of code that decides what reaches a SQL statement. Not worth trading typed code for a dependency this small.

Nothing in filtrify's existing `pkg/lang` is a starting point — `pkg/lang/sql` was brainstorming, not a prototype. The translator should be written fresh against the schema metadata below.

## Translation

| AST | SQL |
|---|---|
| `Comparison{field, operator, value}` | `<column> <op> ?` |
| `AndExpr` / `OrExpr` | `(a AND b)` / `(a OR b)` |
| `NotExpr` | `NOT (a)` |
| `HasChildren{entity, with}` | correlated `EXISTS` |

`HasChildren` is the only one that is not local:

```sql
EXISTS (
  SELECT 1 FROM employment
  WHERE employment.person_id = person.id
    AND employment.title = ?
)
```

The child table and join columns come from the registry: find the table whose `entity` matches, then the foreign key on it whose `ref_table` is the parent. It stays a subquery rather than a join, so it neither multiplies parent rows nor interferes with the batched-`IN` eager loading, which runs afterwards on the already-filtered parents.

`query.Conn.renumber` already renumbers `?` across a whole statement, so nesting needs no special handling.

## Should a library build the SQL?

Measured, rather than assumed:

| | modules pulled | external pkgs linked | state |
|---|---|---|---|
| `huandu/go-sqlbuilder` | 10 | 4 | actively released (v1.43.0) |
| `Masterminds/squirrel` | 6 | 4 | maintenance mode, no development since 2024 |
| `doug-martin/goqu/v9` | 17 | 9 | active, known for feature sprawl |
| `stephenafamo/bob` | 130 | — | ORM/codegen, far past what is needed |

What it would displace is smaller than it looks. Of `pkg/query`'s 250 lines, roughly a quarter is SQL assembly (`Cond`, `In`, `Select`, `renumber`). `Mask` and `Key` are domain-specific — no builder parses a FieldMask into a column list, or reconciles a driver's `[]byte` with a getter's `string` — and those are the two that have actually had bugs.

Where a builder does earn its keep is exactly this document's work: nested `AND`/`OR`/`NOT` parenthesisation, `EXISTS` subqueries, and per-dialect identifier quoting — the last being the trap the subtype CHECK constraints already fell into on MySQL.

The catch is that adopting one for filters only means two SQL-assembly mechanisms in one statement. It is all or nothing: either `Conn.Select` is rebuilt on the library too, or the translator stays hand-rolled and reuses `renumber`.

Either way it breaks the two-dependency rule, so it is a decision, not a detail. If taken, `go-sqlbuilder` is the pick — light, actively maintained, explicit dialect flavors.

## Safety

The one invariant: **only strings resolved from generated metadata reach the SQL text.**

- `Comparison.field` resolves through the table's columns, or the build fails. Never interpolated.
- `Comparison.operator` maps through a fixed whitelist, or the build fails.
- Every `Value` becomes a bind parameter.

A filter arrives from a client, so an unresolvable field or operator has to be an error, never a passthrough.

## Findings in filtrify worth fixing first

**Unknown operators silently become equality.** `ComparisonNode.Expr()` initialises `op := "eq"` and its switch has no default, so `a ~ b` parses as `a = b` rather than failing. Any operator the SQL whitelist would reject is already lost before the AST is built, so the two layers have to agree on the vocabulary or the parser will quietly widen a filter.

**Operator vocabulary is split.** The grammar accepts `=`, `!=`, `<`, `<=`, `>`, `>=`; the builder normalises to `eq`, `neq`, `gt`, `gte`, `lt`, `lte`, and `in` and `contains` appear only from other rules. The canonical set should be written down before the SQL side whitelists it.

## Open questions

1. **`HasChildren.entity` — entity name or relation field name?** They differ when a parent has two relations to the same entity (`Person.employments` and, say, `Person.past_employments`). An entity name cannot disambiguate.
2. **`contains` semantics.** `LIKE '%x%'` needs `%` and `_` escaped in the value, and the escape character differs per dialect. Case sensitivity also differs (`ILIKE` on PostgreSQL, collation-dependent on MySQL).
3. **`IsNullExpr` is declared but not in `Expr`'s oneof.** Either wire it in or drop it; `field = NULL` is never what a caller means.
4. **Ordering and pagination** are adjacent (filtrify has a paginate service) and would need the same field→column resolution. Worth deciding whether they ride along.
