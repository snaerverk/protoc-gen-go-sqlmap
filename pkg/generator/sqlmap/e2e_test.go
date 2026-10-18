package sqlmap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// relationDriverSrc asserts what only a live database confirms: nullability
// and ON DELETE SET NULL.
const relationDriverSrc = `package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"e2etest/relationpb"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/migration"
)

func run(ctx context.Context, db *sql.DB) error {
	m, err := migration.New(db, migration.DialectPostgres)
	if err != nil {
		return err
	}
	if err := m.Create(ctx, relationpb.AuthorTable, relationpb.BookTable); err != nil {
		return fmt.Errorf("Create: %w", err)
	}

	var authorID int64
	if err := db.QueryRowContext(ctx,
		"INSERT INTO tbl_author (author_name) VALUES ($1) RETURNING author_id", "Ursula",
	).Scan(&authorID); err != nil {
		return fmt.Errorf("insert author: %w", err)
	}
	var bookID int64
	if err := db.QueryRowContext(ctx,
		"INSERT INTO tbl_book (book_title, author_id) VALUES ($1, $2) RETURNING book_id", "Earthsea", authorID,
	).Scan(&bookID); err != nil {
		return fmt.Errorf("insert book: %w", err)
	}

	// author_id has proto presence and is not a PK, so it must be nullable.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO tbl_book (book_title, author_id) VALUES ($1, NULL)", "Orphan",
	); err != nil {
		return fmt.Errorf("insert NULL author_id, so the column was generated NOT NULL: %w", err)
	}
	// book_id is a PK: it must have been generated NOT NULL.
	if _, err := db.ExecContext(ctx, "INSERT INTO tbl_book (book_id) VALUES (NULL)"); err == nil {
		return fmt.Errorf("inserting NULL into the primary key succeeded, so it was generated nullable")
	}
	// Deleting the author must NULL the referencing column rather than fail,
	// which only happens if ON DELETE SET NULL made it into the DDL.
	if _, err := db.ExecContext(ctx, "DELETE FROM tbl_author WHERE author_id = $1", authorID); err != nil {
		return fmt.Errorf("delete author (ON DELETE SET NULL not applied?): %w", err)
	}

	cols := relationpb.GetBookColumns()
	row := db.QueryRowContext(ctx,
		"SELECT "+strings.Join(cols, ", ")+" FROM tbl_book WHERE book_id = $1", bookID)
	var result relationpb.BookResult
	if err := result.Scan(cols, row); err != nil {
		return fmt.Errorf("Scan: %w", err)
	}
	if result.GetTitle() != "Earthsea" {
		return fmt.Errorf("Title = %q, want %q", result.GetTitle(), "Earthsea")
	}
	if result.AuthorId != nil {
		return fmt.Errorf("AuthorId = %d, want NULL after the author was deleted", *result.AuthorId)
	}
	return nil
}
`

// eagerDriverSrc exercises the query writer: mask-driven column selection and
// eager loading two levels deep in both directions.
const eagerDriverSrc = `package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"e2etest/eagerpb"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/migration"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/query"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// UTC and whole seconds, so the comparison ignores precision and time zone.
var published = time.Date(1968, 11, 1, 12, 0, 0, 0, time.UTC)

func run(ctx context.Context, db *sql.DB) error {
	m, err := migration.New(db, migration.DialectPostgres)
	if err != nil {
		return err
	}
	if err := m.Create(ctx, eagerpb.PublisherTable, eagerpb.AuthorTable, eagerpb.BookTable); err != nil {
		return fmt.Errorf("Create: %w", err)
	}

	var pubID, authorID int64
	if err := db.QueryRowContext(ctx,
		"INSERT INTO tbl_publisher (publisher_name) VALUES ($1) RETURNING publisher_id", "Gollancz",
	).Scan(&pubID); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx,
		"INSERT INTO tbl_author (author_name) VALUES ($1) RETURNING author_id", "Ursula",
	).Scan(&authorID); err != nil {
		return err
	}
	// Earthsea carries a timestamp; Lathe leaves published_at NULL, so the
	// scanner is exercised on both.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO tbl_book (book_title, author_id, publisher_id, published_at) VALUES ($1, $2, $3, $4)",
		"Earthsea", authorID, pubID, published); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO tbl_book (book_title, author_id, publisher_id) VALUES ($1, $2, $3)",
		"Lathe", authorID, pubID); err != nil {
		return err
	}
	// A second author with no books, to prove eager loading does not invent one.
	if _, err := db.ExecContext(ctx, "INSERT INTO tbl_author (author_name) VALUES ($1)", "Nobody"); err != nil {
		return err
	}

	conn := query.Conn{DB: db, Dialect: query.Postgres}

	// Two levels of nesting, across both relation directions.
	authors, err := eagerpb.LoadAuthor(ctx, conn,
		&fieldmaskpb.FieldMask{Paths: []string{"name", "books.title", "books.publisher.name"}})
	if err != nil {
		return fmt.Errorf("LoadAuthor: %w", err)
	}
	if len(authors) != 2 {
		return fmt.Errorf("got %d authors, want 2", len(authors))
	}
	byName := map[string]*eagerpb.Author{}
	for _, a := range authors {
		byName[a.GetName()] = a
	}
	ursula, ok := byName["Ursula"]
	if !ok {
		return fmt.Errorf("author Ursula missing; got %v", byName)
	}
	if len(ursula.GetBooks()) != 2 {
		return fmt.Errorf("Ursula has %d books, want 2", len(ursula.GetBooks()))
	}
	if got := byName["Nobody"].GetBooks(); len(got) != 0 {
		return fmt.Errorf("Nobody has %d books, want 0", len(got))
	}
	for _, b := range ursula.GetBooks() {
		if b.GetTitle() == "" {
			return fmt.Errorf("book title was masked in but came back empty")
		}
		if b.GetPublisher().GetName() != "Gollancz" {
			return fmt.Errorf("nested publisher name = %q, want %q", b.GetPublisher().GetName(), "Gollancz")
		}
	}

	// A google.protobuf.Timestamp column must round-trip through the driver's
	// time.Time, and a NULL one must stay nil rather than become the zero
	// instant.
	books, err := eagerpb.LoadBook(ctx, conn,
		&fieldmaskpb.FieldMask{Paths: []string{"title", "published_at"}})
	if err != nil {
		return fmt.Errorf("LoadBook: %w", err)
	}
	seen := map[string]*eagerpb.Book{}
	for _, b := range books {
		seen[b.GetTitle()] = b
	}
	got := seen["Earthsea"].GetPublishedAt()
	if got == nil {
		return fmt.Errorf("published_at came back nil for Earthsea")
	}
	if !got.AsTime().Equal(published) {
		return fmt.Errorf("published_at = %s, want %s", got.AsTime(), published)
	}
	if ts := seen["Lathe"].PublishedAt; ts != nil {
		return fmt.Errorf("a NULL published_at should stay nil, got %s", ts.AsTime())
	}

	// A mask that does not name the relation must not load it, and must not
	// select the columns it left out either.
	authors, err = eagerpb.LoadAuthor(ctx, conn, &fieldmaskpb.FieldMask{Paths: []string{"name"}})
	if err != nil {
		return fmt.Errorf("LoadAuthor(name): %w", err)
	}
	for _, a := range authors {
		if len(a.GetBooks()) != 0 {
			return fmt.Errorf("books were loaded despite not being in the mask")
		}
	}

	// Conversely, masking only the relation must leave the scalar unselected.
	authors, err = eagerpb.LoadAuthor(ctx, conn, &fieldmaskpb.FieldMask{Paths: []string{"books.title"}})
	if err != nil {
		return fmt.Errorf("LoadAuthor(books.title): %w", err)
	}
	for _, a := range authors {
		if a.GetName() != "" {
			return fmt.Errorf("author_name = %q was selected despite not being in the mask", a.GetName())
		}
	}

	// A nil mask selects every column but loads no relations: eager loading is
	// opt-in, so that "no mask" cannot walk the whole object graph.
	authors, err = eagerpb.LoadAuthor(ctx, conn, nil)
	if err != nil {
		return fmt.Errorf("LoadAuthor(nil): %w", err)
	}
	for _, a := range authors {
		if a.GetName() == "" {
			return fmt.Errorf("a nil mask should select every column, but author_name was empty")
		}
		if len(a.GetBooks()) != 0 {
			return fmt.Errorf("a nil mask loaded %d books; relations must be opt-in", len(a.GetBooks()))
		}
	}

	// A leaf path selects the whole relation, but must not cascade into the
	// relations below it.
	authors, err = eagerpb.LoadAuthor(ctx, conn, &fieldmaskpb.FieldMask{Paths: []string{"books"}})
	if err != nil {
		return fmt.Errorf("LoadAuthor(books): %w", err)
	}
	for _, a := range authors {
		for _, b := range a.GetBooks() {
			if b.GetTitle() == "" {
				return fmt.Errorf("a leaf relation path should select all of its columns")
			}
			if b.GetPublisher() != nil {
				return fmt.Errorf("a leaf relation path must not cascade into nested relations")
			}
		}
	}
	return nil
}
`

// subtypeDriverSrc checks what the database itself rejects, which is the whole
// point of the discriminator plus composite foreign key.
const subtypeDriverSrc = `package main

import (
	"context"
	"database/sql"
	"fmt"

	"e2etest/subtypepb"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/migration"
)

func run(ctx context.Context, db *sql.DB) error {
	m, err := migration.New(db, migration.DialectPostgres)
	if err != nil {
		return err
	}
	if err := m.Create(ctx, subtypepb.IdentityTable, subtypepb.PersonTable, subtypepb.OrganisationTable); err != nil {
		return fmt.Errorf("Create: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO identity (id, kind) VALUES ('i1','person'), ('i2','org')"); err != nil {
		return fmt.Errorf("insert identities: %w", err)
	}
	// kind is defaulted, so a subtype insert never has to name it.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO person (id, given_name) VALUES ('i1','Ursula')"); err != nil {
		return fmt.Errorf("insert person: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO organisation (id, name) VALUES ('i2','ACME')"); err != nil {
		return fmt.Errorf("insert organisation: %w", err)
	}

	mustFail := func(what, stmt string) error {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			return fmt.Errorf("%s was accepted, but the schema must reject it", what)
		}
		return nil
	}
	// The identity is a person, so it cannot also be an organisation. This is
	// the guarantee the whole pattern exists for.
	if err := mustFail("a second subtype row for one identity",
		"INSERT INTO organisation (id, name) VALUES ('i1','Bogus')"); err != nil {
		return err
	}
	if err := mustFail("an unknown discriminator value",
		"INSERT INTO identity (id, kind) VALUES ('i3','robot')"); err != nil {
		return err
	}
	if err := mustFail("a subtype row carrying another subtype's discriminator",
		"INSERT INTO person (id, kind, given_name) VALUES ('i2','org','X')"); err != nil {
		return err
	}
	if err := mustFail("re-pointing an identity while its subtype row exists",
		"UPDATE identity SET kind='org' WHERE id='i1'"); err != nil {
		return err
	}

	// ON DELETE CASCADE reaches the subtype through the composite key.
	if _, err := db.ExecContext(ctx, "DELETE FROM identity WHERE id='i1'"); err != nil {
		return fmt.Errorf("delete identity: %w", err)
	}
	var people int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM person").Scan(&people); err != nil {
		return err
	}
	if people != 0 {
		return fmt.Errorf("deleting the identity left %d person row(s) behind", people)
	}
	return nil
}
`

// mainSrc wraps a driver's run() with connection setup.
const mainSrc = `package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	ctx := context.Background()
	db, err := sql.Open("postgres", os.Getenv("SQLMAP_DSN"))
	if err != nil {
		fail(err)
	}
	defer db.Close()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := db.PingContext(ctx); err == nil {
			break
		} else if time.Now().After(deadline) {
			fail(fmt.Errorf("database never became reachable: %w", err))
		}
		time.Sleep(500 * time.Millisecond)
	}

	if err := run(ctx, db); err != nil {
		fail(err)
	}
	fmt.Println("OK")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
`

const (
	pgImage    = "postgres:16-alpine"
	pgUser     = "sqlmap"
	pgPassword = "sqlmap"
	pgDatabase = "sqlmap"
)

// startPostgres runs a throwaway container, removed when the test finishes.
func startPostgres(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH")
	}

	// Output(), not CombinedOutput(): pull progress on stderr corrupts the id.
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-e", "POSTGRES_USER="+pgUser,
		"-e", "POSTGRES_PASSWORD="+pgPassword,
		"-e", "POSTGRES_DB="+pgDatabase,
		"-P", pgImage,
	).Output()
	if err != nil {
		t.Skipf("could not start %s: %v", pgImage, err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	port, err := hostPort(id)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
		pgUser, pgPassword, port, pgDatabase)
}

// hostPort resolves the host port docker mapped the container's 5432 to.
func hostPort(id string) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := exec.Command("docker", "port", id, "5432/tcp").Output()
		if err == nil {
			// One "addr:port" mapping per line (IPv4 and IPv6).
			for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
				if i := strings.LastIndex(line, ":"); i >= 0 {
					return strings.TrimSpace(line[i+1:]), nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("container %s never published port 5432: %v", id, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// runE2E generates protoFile through the real pipeline, assembles it into a
// throwaway Go module with driverSrc, and runs it against a fresh PostgreSQL.
// The SQL driver belongs to that module, not this repo.
func runE2E(t *testing.T, protoFile, pkgName, driverSrc string) {
	t.Helper()
	runE2EWith(t, protoFile, pkgName, driverSrc, "")
}

// runE2EWith is runE2E with a plugins= value ("" for the default) and extra
// module requirements for the throwaway module, such as filtrify.
func runE2EWith(t *testing.T, protoFile, pkgName, driverSrc, plugins string, require ...string) {
	t.Helper()
	if os.Getenv("SQLMAP_E2E") == "" {
		t.Skip("set SQLMAP_E2E=1 to run (needs docker, protoc, protoc-gen-go, and module-cache access)")
	}
	for _, bin := range []string{"protoc", "protoc-gen-go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found in PATH", bin)
		}
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	testdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(protoFile, ".proto")
	dsn := startPostgres(t)

	// 1. Base message types, via the real protoc-gen-go binary.
	pbOutDir := t.TempDir()
	cmd := exec.Command("protoc",
		"-I", filepath.Join(repoRoot, "proto"),
		"-I", testdata,
		"--go_out=paths=source_relative,"+
			"Msqlmap/v1/sqlmap.proto=github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generated/sqlmap/v1,"+
			"Mschema/v1/schema.proto=github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generated/schema/v1:"+pbOutDir,
		filepath.Join(testdata, protoFile),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("protoc --go_out: %v\n%s", err, out)
	}
	basePB, err := os.ReadFile(filepath.Join(pbOutDir, base+".pb.go"))
	if err != nil {
		t.Fatal(err)
	}

	// 2. Schema/scanner/query code, via our own generator, in-process.
	param := ""
	if plugins != "" {
		param = "plugins=" + plugins
	}
	files := generateWith(t, protoFile, param)
	generated, ok := files[base+".sqlmap.go"]
	if !ok {
		t.Fatalf("no %s.sqlmap.go in generated output, got files: %v", base, fileNames(files))
	}

	// 3. A throwaway module, replaced back at this checkout so pkg/migration
	// and pkg/query resolve to the code under test.
	modRoot := t.TempDir()
	pkgDir := filepath.Join(modRoot, pkgName)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{
		filepath.Join(pkgDir, base+".pb.go"):     basePB,
		filepath.Join(pkgDir, base+".sqlmap.go"): []byte(generated),
		filepath.Join(modRoot, "main.go"):        []byte(mainSrc),
		filepath.Join(modRoot, "driver.go"):      []byte(driverSrc),
		filepath.Join(modRoot, "go.mod"):         []byte("module e2etest\n\ngo 1.27\n"),
	} {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run := func(name string, args ...string) string {
		t.Helper()
		c := exec.Command(name, args...)
		c.Dir = modRoot
		c.Env = append(os.Environ(), "SQLMAP_DSN="+dsn)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}
	edit := []string{"mod", "edit",
		"-require=github.com/snaerverk/protoc-gen-go-sqlmap@v0.0.0",
		"-replace=github.com/snaerverk/protoc-gen-go-sqlmap=" + repoRoot,
	}
	for _, r := range require {
		edit = append(edit, "-require="+r)
	}
	run("go", edit...)
	run("go", "mod", "tidy")

	if out := run("go", "run", "."); out != "OK\n" {
		t.Fatalf("driver program did not report success, got: %q", out)
	}
}

// TestE2E_MigrationCreateAndScan covers the schema writer and pkg/migration.
func TestE2E_MigrationCreateAndScan(t *testing.T) {
	runE2E(t, "relation.proto", "relationpb", relationDriverSrc)
}

// TestE2E_EagerLoading covers the query writer against a live database.
func TestE2E_EagerLoading(t *testing.T) {
	runE2E(t, "eager.proto", "eagerpb", eagerDriverSrc)
}

// TestE2E_SubtypeConstraints covers joined-table subtypes end to end.
func TestE2E_SubtypeConstraints(t *testing.T) {
	runE2E(t, "subtype.proto", "subtypepb", subtypeDriverSrc)
}

// filtrifyModule is what the filter plugin's output imports. It lives in the
// throwaway module only, never in this repo's go.mod.
const filtrifyModule = "github.com/snaerverk/filtrify@v0.0.2-0.20261018093115-89a6b4659be6"

// TestE2E_Filter covers the filter writer: the generated schemas translate
// filtrify expressions that sqlmap's loaders run, and page by keyset.
func TestE2E_Filter(t *testing.T) {
	runE2EWith(t, "filter.proto", "filterpb", filterDriverSrc, "schema+scanner+query+filter", filtrifyModule)
}

// filterDriverSrc runs the generated filter schemas against PostgreSQL.
const filterDriverSrc = `package main

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"e2etest/filterpb"

	"github.com/snaerverk/filtrify/pkg/filter"
	fsql "github.com/snaerverk/filtrify/pkg/filter/sql"
	paginatev1 "github.com/snaerverk/filtrify/pkg/generated/filtrify/paginate/v1"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/migration"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/query"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func day(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }

func run(ctx context.Context, db *sql.DB) error {
	m, err := migration.New(db, "postgres")
	if err != nil {
		return err
	}
	if err := m.Create(ctx, filterpb.TeamTable, filterpb.MemberTable, filterpb.BadgeTable); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	// Teams 1 and 3 tie on founded; team 2's size is NULL.
	for _, tm := range []struct {
		id      int64
		name    string
		active  bool
		founded time.Time
		size    any
	}{
		{1, "Acme", true, day(1), 10},
		{2, "acme_50%", false, day(2), nil},
		{3, "Beta", true, day(1), 30},
		{4, "Gamma", false, day(3), 20},
	} {
		if _, err := db.ExecContext(ctx, "INSERT INTO tbl_team (team_id, team_name, active, founded, size) VALUES ($1, $2, $3, $4, $5)",
			tm.id, tm.name, tm.active, tm.founded, tm.size); err != nil {
			return err
		}
	}
	for _, mb := range [][2]any{{"m1", 1}, {"m2", 1}, {"m3", 3}} {
		if _, err := db.ExecContext(ctx, "INSERT INTO tbl_member (member_id, member_team) VALUES ($1, $2)", mb[0], mb[1]); err != nil {
			return err
		}
	}
	conn := query.Conn{DB: db, Dialect: query.Postgres}

	// Filters run through sqlmap's own loader, which renumbers filtrify's
	// "?" placeholders for PostgreSQL alongside its own.
	teams := func(f string) ([]int64, error) {
		expr, err := filter.Parse(ctx, f)
		if err != nil {
			return nil, err
		}
		cond, err := filterpb.TeamFilter.Cond(expr)
		if err != nil {
			return nil, err
		}
		rows, err := filterpb.LoadTeam(ctx, conn, &fieldmaskpb.FieldMask{Paths: []string{"name"}}, query.Cond(cond))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		var ids []int64
		for _, r := range rows {
			ids = append(ids, r.GetId())
		}
		slices.Sort(ids)
		return ids, nil
	}
	for f, want := range map[string][]int64{
		` + "`contains(name, \"ACME\")`" + `:                         {1, 2},
		` + "`contains(name, \"_50%\")`" + `:                         {2},
		` + "`active = true AND founded = \"2026-01-01\"`" + `:       {1, 3},
		` + "`size >= 20`" + `:                                      {3, 4},
		` + "`has(members, id = \"m3\")`" + `:                       {3},
		` + "`NOT has(members, id != \"\")`" + `:                    {2, 4},
		` + "`in(id, [2, 4]) OR name = \"Beta\"`" + `:                {2, 3, 4},
		` + "`parent = 1`" + `:                                      nil,
	} {
		got, err := teams(f)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("teams %s: got %v, want %v", f, got, want)
		}
	}

	// belongs-to, and back down through the same table.
	members := func(f string) ([]string, error) {
		expr, err := filter.Parse(ctx, f)
		if err != nil {
			return nil, err
		}
		cond, err := filterpb.MemberFilter.Cond(expr)
		if err != nil {
			return nil, err
		}
		rows, err := filterpb.LoadMember(ctx, conn, nil, query.Cond(cond))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		var ids []string
		for _, r := range rows {
			ids = append(ids, r.GetId())
		}
		slices.Sort(ids)
		return ids, nil
	}
	for f, want := range map[string][]string{
		` + "`has(team, name = \"Acme\")`" + `:                              {"m1", "m2"},
		` + "`has(team, active = true AND has(members, id = \"m3\"))`" + `: {"m3"},
		` + "`team = 1`" + `:                                                {"m1", "m2"},
	} {
		got, err := members(f)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("members %s: got %v, want %v", f, got, want)
		}
	}

	// Keyset pages: founded desc, ties by name.
	order := []*paginatev1.OrderField{
		{Field: "founded", Direction: paginatev1.Direction_DIRECTION_DESC},
		{Field: "name"},
	}
	for _, size := range []int32{1, 2, 3} {
		var got []int64
		after := ""
		for {
			page, err := filterpb.TeamFilter.Page(order, &paginatev1.Paging{First: size, After: after}, "name")
			if err != nil {
				return err
			}
			var conds []fsql.Cond
			if c, ok := page.After(); ok {
				conds = append(conds, c)
			}
			where, args := fsql.Where(conds...)
			stmt := fmt.Sprintf("SELECT tbl_team.team_id, %s FROM tbl_team%s ORDER BY %s LIMIT %d",
				strings.Join(page.Columns(), ", "), where, page.OrderBy(), page.Size()+1)
			rows, err := db.QueryContext(ctx, fsql.Rebind(fsql.Postgres, stmt), args...)
			if err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
			n, last, more := 0, "", false
			for rows.Next() {
				if n == page.Size() {
					more = true
					break
				}
				var id int64
				dest := page.Dest()
				if err := rows.Scan(append([]any{&id}, dest...)...); err != nil {
					return err
				}
				got = append(got, id)
				last = page.Cursor(dest)
				n++
			}
			rows.Close()
			if !more {
				break
			}
			after = last
		}
		if want := []int64{4, 2, 1, 3}; !slices.Equal(got, want) {
			return fmt.Errorf("pages of %d: got %v, want %v", size, got, want)
		}
	}

	// A composite key can be filtered but not paged.
	if _, err := filterpb.BadgeFilter.Page(nil, nil, "label"); err == nil {
		return fmt.Errorf("Badge paginated without a Key")
	}
	return nil
}
`

// TestE2E_Page covers the page plugin and pkg/page: Query<Msg> walks pages of
// a search with the relations the mask selects.
func TestE2E_Page(t *testing.T) {
	runE2EWith(t, "filter.proto", "filterpb", pageDriverSrc, "schema+scanner+query+filter+page", filtrifyModule)
}

// pageDriverSrc runs the generated Query<Msg> functions against PostgreSQL.
const pageDriverSrc = `package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"e2etest/filterpb"

	"github.com/snaerverk/filtrify/pkg/filter"
	paginatev1 "github.com/snaerverk/filtrify/pkg/generated/filtrify/paginate/v1"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/migration"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/page"
	"github.com/snaerverk/protoc-gen-go-sqlmap/pkg/query"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func day(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }

func run(ctx context.Context, db *sql.DB) error {
	m, err := migration.New(db, "postgres")
	if err != nil {
		return err
	}
	if err := m.Create(ctx, filterpb.TeamTable, filterpb.MemberTable, filterpb.BadgeTable); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	// Seven teams; founded ties in pairs so pages split ties.
	for i := int64(1); i <= 7; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO tbl_team (team_id, team_name, active, founded) VALUES ($1, $2, $3, $4)",
			i, fmt.Sprintf("team-%d", i), i%2 == 1, day(int((i+1)/2))); err != nil {
			return err
		}
	}
	for _, mb := range [][2]any{{"m1", 1}, {"m2", 1}, {"m3", 3}, {"m4", 7}} {
		if _, err := db.ExecContext(ctx, "INSERT INTO tbl_member (member_id, member_team) VALUES ($1, $2)", mb[0], mb[1]); err != nil {
			return err
		}
	}
	conn := query.Conn{DB: db, Dialect: query.Postgres}

	walk := func(req page.Request, size int32, mask *fieldmaskpb.FieldMask) ([]*filterpb.Team, int, error) {
		var all []*filterpb.Team
		total := -1
		after := ""
		for range 20 {
			req.Paging = &paginatev1.Paging{First: size, After: after}
			res, err := filterpb.QueryTeam(ctx, conn, req, mask)
			if err != nil {
				return nil, 0, err
			}
			if total >= 0 && res.Total != total {
				return nil, 0, fmt.Errorf("total changed between pages: %d, %d", total, res.Total)
			}
			total = res.Total
			if res.Page.HasPrevious != (after != "") {
				return nil, 0, fmt.Errorf("has previous %v after %q", res.Page.HasPrevious, after)
			}
			all = append(all, res.Rows...)
			if !res.Page.HasNext {
				return all, total, nil
			}
			after = res.Page.LastCursor
		}
		return nil, 0, fmt.Errorf("no last page")
	}
	ids := func(teams []*filterpb.Team) []int64 {
		var out []int64
		for _, tm := range teams {
			out = append(out, tm.GetId())
		}
		return out
	}

	// Default order: the key.
	for _, size := range []int32{1, 3, 7, 50} {
		teams, total, err := walk(page.Request{}, size, nil)
		if err != nil {
			return err
		}
		if got, want := ids(teams), []int64{1, 2, 3, 4, 5, 6, 7}; !slices.Equal(got, want) || total != 7 {
			return fmt.Errorf("default order, size %d: %v (total %d)", size, got, total)
		}
	}

	// Search + order with ties, relations loaded through the mask.
	expr, err := filter.Parse(ctx, ` + "`active = true OR has(members, id = \"m4\")`" + `)
	if err != nil {
		return err
	}
	req := page.Request{
		Search: expr,
		Order:  []*paginatev1.OrderField{{Field: "founded", Direction: paginatev1.Direction_DIRECTION_DESC}},
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"name", "members.id"}}
	for _, size := range []int32{1, 2, 4} {
		teams, total, err := walk(req, size, mask)
		if err != nil {
			return err
		}
		// Matches 1, 3, 5, 7; founded desc, ties by key desc (the key
		// follows the last field's direction).
		if got, want := ids(teams), []int64{7, 5, 3, 1}; !slices.Equal(got, want) || total != 4 {
			return fmt.Errorf("search, size %d: %v (total %d)", size, got, total)
		}
		var members []string
		for _, tm := range teams {
			for _, mb := range tm.GetMembers() {
				members = append(members, mb.GetId())
			}
		}
		slices.Sort(members)
		if want := []string{"m1", "m2", "m3", "m4"}; !slices.Equal(members, want) {
			return fmt.Errorf("members loaded: %v", members)
		}
		if teams[0].GetName() != "team-7" || teams[0].Active != nil {
			return fmt.Errorf("mask: got name %q, active %v", teams[0].GetName(), teams[0].Active)
		}
	}

	// Trusted conditions combine with the search, and with the count.
	req.Conds = []query.Cond{{SQL: "tbl_team.team_id <= ?", Args: []any{int64(3)}}}
	teams, total, err := walk(req, 1, nil)
	if err != nil {
		return err
	}
	if got, want := ids(teams), []int64{3, 1}; !slices.Equal(got, want) || total != 2 {
		return fmt.Errorf("conds: %v (total %d)", got, total)
	}

	// A string key, ordered by a belongs-to column.
	res, err := filterpb.QueryMember(ctx, conn, page.Request{
		Order: []*paginatev1.OrderField{{Field: "team", Direction: paginatev1.Direction_DIRECTION_DESC}},
	}, &fieldmaskpb.FieldMask{Paths: []string{"team.name"}})
	if err != nil {
		return err
	}
	var got []string
	for _, mb := range res.Rows {
		got = append(got, mb.GetId()+"/"+mb.GetTeam().GetName())
	}
	if want := []string{"m4/team-7", "m3/team-3", "m2/team-1", "m1/team-1"}; !slices.Equal(got, want) {
		return fmt.Errorf("members by team desc: %v", got)
	}

	// Client mistakes are filter.ErrInvalid; nothing is queried for them.
	bad, _ := filter.Parse(ctx, ` + "`nope = 1`" + `)
	for name, r := range map[string]page.Request{
		"unknown field":  {Search: bad},
		"unknown order":  {Order: []*paginatev1.OrderField{{Field: "motto"}}},
		"garbage cursor": {Paging: &paginatev1.Paging{After: "x"}},
	} {
		if _, err := filterpb.QueryTeam(ctx, conn, r, nil); !errors.Is(err, filter.ErrInvalid) {
			return fmt.Errorf("%s: err = %v, want filter.ErrInvalid", name, err)
		}
	}
	return nil
}
`
