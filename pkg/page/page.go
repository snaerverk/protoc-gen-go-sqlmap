// Package page runs a paged query over a generated table: a filtrify search,
// an order and a keyset cursor select one page of keys, the generated loader
// fetches those rows with the relations the mask asks for, and a count gives
// the total across pages. Generated Query<Msg> functions (the `page` plugin)
// fill in Table; call Query directly for anything they don't cover.
package page

import (
	"context"
	"fmt"
	"strings"

	"github.com/snaerverk/filtrify/pkg/filter"
	fsql "github.com/snaerverk/filtrify/pkg/filter/sql"
	astv1 "github.com/snaerverk/filtrify/pkg/generated/filtrify/ast/v1"
	paginatev1 "github.com/snaerverk/filtrify/pkg/generated/filtrify/paginate/v1"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/query"
)

// Request is a search, an order and a position. Zero values mean no filter,
// the table's default order and the first page of filter.DefaultPageSize.
type Request struct {
	Search *astv1.Expr
	Order  []*paginatev1.OrderField
	Paging *paginatev1.Paging
	// Conds are ANDed with Search, e.g. to scope rows to a tenant. Unlike
	// Search they are trusted SQL, never client input.
	Conds []query.Cond
}

// Result is one page of rows.
type Result[T any] struct {
	Rows []T
	// Total counts all rows matching the request, across pages.
	Total int
	Page  filter.PageInfo
}

// Loader is a generated Load<Msg>Rows.
type Loader[T any] func(ctx context.Context, c query.Conn, m *query.Mask, conds []query.Cond, extra ...string) ([]T, error)

// Table is what Query needs from the generated code of one table.
type Table[T any] struct {
	// Name is the SQL table.
	Name string
	// Schema is the generated <Msg>Filter. It must have a Key.
	Schema *fsql.Schema
	// DefaultOrder is the field to order by when the request has none;
	// fsql.KeyField orders by the key alone.
	DefaultOrder string
	Load         Loader[T]
	// Key reads Schema.Key off a loaded row.
	Key func(T) any
}

// Query returns one page of t's rows matching req, loaded with m. Invalid
// searches, orders and cursors are errors wrapping filter.ErrInvalid.
//
// It runs three statements: the count, the page of keys, and the load of
// those keys. They don't share a snapshot, so a concurrent write can make
// Total disagree with the pages by a row, and a row deleted in between is
// left out of its page.
func Query[T any](ctx context.Context, c query.Conn, t Table[T], req Request, m *query.Mask) (*Result[T], error) {
	pg, err := t.Schema.Page(req.Order, req.Paging, t.DefaultOrder)
	if err != nil {
		return nil, err
	}
	conds := make([]fsql.Cond, 0, len(req.Conds)+2)
	for _, cond := range req.Conds {
		conds = append(conds, fsql.Cond(cond))
	}
	if req.Search != nil {
		cond, err := t.Schema.Cond(req.Search)
		if err != nil {
			return nil, err
		}
		conds = append(conds, cond)
	}

	res := &Result[T]{Page: filter.PageInfo{HasPrevious: pg.HasPrevious()}}
	where, args := fsql.Where(conds...)
	if err := queryRow(ctx, c, "SELECT COUNT(*) FROM "+t.Name+where, args, &res.Total); err != nil {
		return nil, fmt.Errorf("page: count %s: %w", t.Name, err)
	}

	// One row more than the page tells whether another follows.
	if after, ok := pg.After(); ok {
		conds = append(conds, after)
	}
	where, args = fsql.Where(conds...)
	stmt := fmt.Sprintf("SELECT %s, %s FROM %s%s ORDER BY %s LIMIT %d",
		t.Schema.Key.SQL, strings.Join(pg.Columns(), ", "), t.Name, where, pg.OrderBy(), pg.Size()+1)
	rows, err := c.DB.QueryContext(ctx, rebind(c, stmt), args...)
	if err != nil {
		return nil, fmt.Errorf("page: keys of %s: %w", t.Name, err)
	}
	defer rows.Close()
	var keys []any
	for rows.Next() {
		if len(keys) == pg.Size() {
			res.Page.HasNext = true
			break
		}
		var key any
		dest := pg.Dest()
		if err := rows.Scan(append([]any{&key}, dest...)...); err != nil {
			return nil, err
		}
		keys = append(keys, query.Key(key))
		cursor := pg.Cursor(dest)
		if res.Page.FirstCursor == "" {
			res.Page.FirstCursor = cursor
		}
		res.Page.LastCursor = cursor
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	in, ok := query.In(t.Schema.Key.SQL, keys)
	if !ok {
		return res, nil
	}
	loaded, err := t.Load(ctx, c, m, []query.Cond{in})
	if err != nil {
		return nil, err
	}
	// The load returns rows in no particular order; restore the page's.
	byKey := make(map[any]T, len(loaded))
	for _, row := range loaded {
		byKey[query.Key(t.Key(row))] = row
	}
	for _, key := range keys {
		if row, ok := byKey[key]; ok {
			res.Rows = append(res.Rows, row)
		}
	}
	return res, nil
}

func queryRow(ctx context.Context, c query.Conn, stmt string, args []any, dest ...any) error {
	rows, err := c.DB.QueryContext(ctx, rebind(c, stmt), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("no row")
	}
	return rows.Scan(dest...)
}

// rebind numbers the "?" placeholders for PostgreSQL.
func rebind(c query.Conn, stmt string) string {
	if c.Dialect == query.Postgres {
		return fsql.Rebind(fsql.Postgres, stmt)
	}
	return stmt
}
