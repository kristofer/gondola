package sqlite

import (
	"database/sql"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"gondola/orm/driver"
	ormsql "gondola/orm/drivers/sql"
	"reflect"
	"strings"
	"time"
)

const placeholders = "?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?"

var (
	sqliteBackend    = &Backend{}
	transformedTypes = []reflect.Type{
		reflect.TypeOf((*time.Time)(nil)),
		reflect.TypeOf((*bool)(nil)),
	}
)

type Backend struct {
}

func (b *Backend) Name() string {
	return "sqlite3"
}

func (b *Backend) Placeholder(n int) string {
	return "?"
}

func (b *Backend) Placeholders(n int) string {
	p := placeholders
	if n > 32 {
		p = strings.Repeat("?,", n)
	}
	return p[:2*n-1]
}

func (b *Backend) Insert(db *sql.DB, m driver.Model, query string, args ...interface{}) (driver.Result, error) {
	return db.Exec(query, args...)
}

func (b *Backend) FieldType(typ reflect.Type, tag driver.Tag) (string, error) {
	switch typ.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "INTEGER", nil
	case reflect.Float32, reflect.Float64:
		return "REAL", nil
	case reflect.String:
		return "TEXT", nil
	case reflect.Struct:
		if typ.Name() == "Time" && typ.PkgPath() == "time" {
			return "INT", nil
		}
	}
	return "", fmt.Errorf("can't map field type %v to a database type", typ)
}

func (b *Backend) FieldOptions(typ reflect.Type, tag driver.Tag) ([]string, error) {
	var opts []string
	if tag.Has("notnull") {
		opts = append(opts, "NOT NULL")
	}
	if tag.Has("primary_key") {
		opts = append(opts, "PRIMARY KEY")
	} else if tag.Has("unique") {
		opts = append(opts, "UNIQUE")
	}
	if tag.Has("auto_increment") {
		opts = append(opts, "AUTOINCREMENT")
	}
	if def := tag.Value("default"); def != "" {
		if typ.Kind() == reflect.String {
			def = "'" + def + "'"
		}
		opts = append(opts, fmt.Sprintf("DEFAULT %s", def))
	}
	return opts, nil
}

func (b *Backend) Transforms() []reflect.Type {
	return transformedTypes
}

func (b *Backend) ScanInt(val int64, goVal *reflect.Value) error {
	switch goVal.Type().Kind() {
	case reflect.Struct:
		goVal.Set(reflect.ValueOf(time.Unix(val, 0).UTC()))
	case reflect.Bool:
		goVal.SetBool(val != 0)
	}
	return nil
}

func (b *Backend) ScanFloat(val float64, goVal *reflect.Value) error {
	return nil
}

func (b *Backend) ScanBool(val bool, goVal *reflect.Value) error {
	return nil
}

func (b *Backend) ScanByteSlice(val []byte, goVal *reflect.Value) error {
	return nil
}

func (b *Backend) ScanString(val string, goVal *reflect.Value) error {
	return nil
}

func (b *Backend) ScanTime(val *time.Time, goVal *reflect.Value) error {
	return nil
}

func (b *Backend) TransformOutValue(val reflect.Value) (interface{}, error) {
	for val.Type().Kind() == reflect.Ptr {
		val = val.Elem()
	}
	switch x := val.Interface().(type) {
	case time.Time:
		if x.IsZero() {
			return nil, nil
		}
		return x.Unix(), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	return nil, fmt.Errorf("can't transform type %v", val.Type())
}

func sqliteOpener(params string) (driver.Driver, error) {
	return ormsql.NewDriver(sqliteBackend, params)
}

func init() {
	driver.Register("sqlite", sqliteOpener)
	driver.Register("sqlite3", sqliteOpener)
}