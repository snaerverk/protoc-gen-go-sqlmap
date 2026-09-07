package types

import (
	"fmt"

	sqlmapv1 "github.com/snaerverk/protoc-gen-go-sqlmap/pkg/generated/sqlmap/v1"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type TableRepo []*Table

func (r TableRepo) GetByName(name string) (*Table, bool) {
	for _, tbl := range r {
		if tbl.GetMessageName() == name {
			return tbl, true
		}
	}
	return nil, false
}

// ForFile returns the tables whose message is declared in f, preserving repo
// order. Writers use it to emit only the tables belonging to the file they are
// currently generating, while still having the full repo for FK lookups.
func (r TableRepo) ForFile(f *protogen.File) []*Table {
	var out []*Table
	for _, tbl := range r {
		for _, msg := range f.Messages {
			if tbl.Msg.Desc.FullName() == msg.Desc.FullName() {
				out = append(out, tbl)
				break
			}
		}
	}
	return out
}

type Table struct {
	Def       *sqlmapv1.Table
	File      *protogen.File
	Msg       *protogen.Message
	columns   []*Column
	relations []*Relation
}

func NewTableFromDescriptor(f *protogen.File, msg *protogen.Message) (*Table, error) {
	ext := proto.GetExtension(msg.Desc.Options(), sqlmapv1.E_Table).(*sqlmapv1.Table)
	if ext == nil {
		return nil, fmt.Errorf("not defined")
	}
	table := &Table{
		Def:  ext,
		File: f,
		Msg:  msg,
	}
	for _, f := range msg.Fields {
		col, err := NewColumn(table, f)
		if err != nil {
			continue
		}
		// A repeated message field carries no value of its own -- the key
		// lives on the rows it points at -- so it is a relation the query
		// writer can load, never a column of this table.
		if f.Desc.IsList() && f.Desc.Kind() == protoreflect.MessageKind && col.Def.ForeignKey != nil {
			table.relations = append(table.relations, &Relation{Table: table, Def: col.Def, Field: f})
			continue
		}
		table.columns = append(table.columns, col)
		// A singular message field with a foreign key is both: the column
		// holding the key, and a relation that can be resolved into the
		// referenced message.
		if f.Desc.Kind() == protoreflect.MessageKind && col.Def.ForeignKey != nil {
			table.relations = append(table.relations, &Relation{Table: table, Def: col.Def, Field: f})
		}
	}
	return table, nil
}

// GetRelations returns the message-kind fields that reference another table.
func (t *Table) GetRelations() []*Relation {
	return t.relations
}

func (t *Table) GetMessageName() string {
	return string(t.Msg.Desc.Name())
}

func (t *Table) GetTableName() string {
	if t.Def.GetName() != "" {
		return t.Def.GetName()
	}
	return t.GetMessageName()
}

func (t *Table) GetColumns() []*Column {
	return t.columns
}

func (t *Table) GetColumnByFieldName(name string) (*Column, bool) {
	for _, col := range t.GetColumns() {
		if col.GetFieldname() == name {
			return col, true
		}
	}
	return nil, false
}

func (t *Table) GetForeignKeys() []*sqlmapv1.ForeignKeyDefinition {
	fks := []*sqlmapv1.ForeignKeyDefinition{}
	for _, fk := range t.Def.ForeignKeys {
		fks = append(fks, fk)
	}
	// Only columns produce schema-level foreign keys. A has-many relation is
	// not in GetColumns() at all: its key lives on the target table, which
	// declares the constraint from its own side.
	for _, c := range t.GetColumns() {
		if c.Def.ForeignKey != nil {
			// Resolve `entity` here rather than passing the raw option
			// through: it is usually left unset, because the message-kind
			// field already names what it references.
			to := &sqlmapv1.ForeignKey{
				Entity:     proto.String(c.GetForeignKeyEntity()),
				Fieldnames: c.Def.ForeignKey.GetFieldnames(),
			}
			fks = append(fks, &sqlmapv1.ForeignKeyDefinition{
				Fieldnames: []string{c.GetFieldname()},
				To:         to,
			})
		}
	}
	return fks
}
