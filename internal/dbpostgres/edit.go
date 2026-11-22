package dbpostgres

// Writing a result's edits back.
//
// **Every value goes out in text, with no type.** A parameter sent with
// type OID 0 is typed by the server from the column it lands in, so the
// user edits the text the server sent and the server reads it back with
// the same input function: an `interval`, an enum, an array, a range,
// `jsonb` — every type there will ever be, and no type table here.
//
// **The `WHERE` is the primary key**, as pgAdmin writes it. A key column
// always has equality, and its value goes out as text like every other,
// typed by the server from the column. A statement that matches no row is
// a row that is gone, and the apply rolls back.
//
// **One transaction, or one savepoint.** Outside a transaction the apply
// is `BEGIN … COMMIT`, all or nothing. Inside one the user holds, it is
// fenced by a savepoint of its own: a refusal rolls back to it and the
// transaction comes out as it went in, and a success commits nothing.

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nick318/meerkat/internal/dbclient"
	"github.com/nick318/meerkat/internal/query"
)

// origins reads where each column came from off the statement's own
// description.
func origins(fields []pgconn.FieldDescription) []dbclient.ColumnOrigin {
	if len(fields) == 0 {
		return nil
	}
	out := make([]dbclient.ColumnOrigin, len(fields))
	for i, field := range fields {
		out[i] = dbclient.ColumnOrigin{Table: field.TableOID, Column: int16(field.TableAttributeNumber)}
	}
	return out
}

// readEditTarget reads one relation: its name, kind, key and columns.
func readEditTarget(ctx context.Context, conn *pgx.Conn, oid uint32) (*dbclient.EditTarget, error) {
	var version int32
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		return nil, fmt.Errorf("failed to read the server version: %w", err)
	}
	target := dbclient.EditTarget{OID: oid, Columns: map[int16]dbclient.EditColumn{}}
	var kind string
	err := conn.QueryRow(ctx,
		"SELECT n.nspname, c.relname, c.relkind::text, "+
			"coalesce((SELECT array_agg(u.k ORDER BY u.o) "+
			"FROM pg_index i, unnest(i.indkey::int2[]) WITH ORDINALITY AS u(k, o) "+
			"WHERE i.indrelid = c.oid AND i.indisprimary), '{}')::int2[] "+
			"FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE c.oid = $1::oid",
		oid).Scan(&target.Schema, &target.Name, &kind, &target.Key)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the table: %w", err)
	}
	if kind != "" {
		target.Kind = kind[0]
	}

	// `attidentity` is PostgreSQL 10 and `attgenerated` 12, as in
	// readCatalog.
	identity, generated := "false", "false"
	if version >= 100000 {
		identity = "a.attidentity = 'a'"
	}
	if version >= 120000 {
		generated = "a.attgenerated <> ''"
	}
	rows, err := conn.Query(ctx,
		"SELECT a.attnum, a.attname, format_type(a.atttypid, a.atttypmod), a.atttypid, "+
			generated+", "+identity+", "+
			"CASE WHEN t.typtype = 'e' THEN "+
			"(SELECT array_agg(e.enumlabel::text ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid) END "+
			"FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid "+
			"WHERE a.attrelid = $1::oid AND a.attnum > 0 AND NOT a.attisdropped",
		oid)
	if err != nil {
		return nil, fmt.Errorf("failed to read the table's columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attnum int16
		var column dbclient.EditColumn
		if err := rows.Scan(&attnum, &column.Name, &column.Type, &column.TypeOID,
			&column.Generated, &column.IdentityAlways, &column.Labels); err != nil {
			return nil, fmt.Errorf("failed to read the table's columns: %w", err)
		}
		column.Bool = column.TypeOID == oidBool
		target.Columns[attnum] = column
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the table's columns: %w", err)
	}
	return &target, nil
}

// applySavepoints numbers the savepoints an apply fences itself off with,
// so a name of the user's own is never the one rolled back to.
var applySavepoints atomic.Uint64

// Apply writes the changes down this session's own connection.
//
// It takes the session's lock, so the caller must not send one while a
// run is out — the shell refuses an apply then, as it refuses a commit.
func (s *PostgresSession) Apply(
	ctx context.Context,
	table *dbclient.EditTarget,
	changes []dbclient.RowChange,
	returning []int16,
) (dbclient.ApplyOutcome, error) {
	conn, err := s.held.lock()
	if err != nil {
		return dbclient.ApplyOutcome{}, err
	}
	defer s.held.release()
	pg := conn.PgConn()

	inside := false
	switch pg.TxStatus() {
	case txFailed:
		return dbclient.ApplyOutcome{}, dbclient.ErrTransactionFailed
	case txOpen:
		inside = true
	}
	savepoint := ""
	if inside {
		savepoint = fmt.Sprintf("meerkat_apply_%d", applySavepoints.Add(1))
		if err := execAll(ctx, pg, "SAVEPOINT "+savepoint); err != nil {
			return dbclient.ApplyOutcome{}, wrapDB(err)
		}
	} else if err := execAll(ctx, pg, "BEGIN"); err != nil {
		return dbclient.ApplyOutcome{}, wrapDB(err)
	}
	// Undoing goes out whatever happened to the context: a cancelled
	// apply must still leave the connection where it found it.
	undo := func() {
		restore, done := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer done()
		if inside {
			_ = execAll(restore, pg, "ROLLBACK TO SAVEPOINT "+savepoint)
			_ = execAll(restore, pg, "RELEASE SAVEPOINT "+savepoint)
			return
		}
		_ = execAll(restore, pg, "ROLLBACK")
	}

	formats := make([]int16, len(returning))
	oids := make([]uint32, len(returning))
	for i, attnum := range returning {
		oids[i] = table.Columns[attnum].TypeOID
	}
	copy(formats, resultFormats(oids))

	rows := make([][]dbclient.Value, len(changes))
	var cut map[dbclient.CellAt]bool
	for i, change := range changes {
		statement := buildChange(table, change, returning, false)
		params := make([][]byte, len(statement.params))
		for j, value := range statement.params {
			params[j] = paramText(value)
		}
		reader := pg.ExecParams(ctx, statement.sql, params, make([]uint32, len(params)), nil, formats)
		var row []dbclient.Value
		var decodeErr error
		for reader.NextRow() {
			if row != nil || decodeErr != nil {
				continue
			}
			fields := reader.FieldDescriptions()
			raw := reader.Values()
			values := make([]dbclient.Value, len(raw))
			for j := range raw {
				value, err := decode(raw[j], fields[j].Format, fields[j].DataTypeOID, j)
				if err != nil {
					decodeErr = err
					break
				}
				values[j] = value.Cap(s.limits.MaxCellBytes)
				if dbclient.WasCut(value, s.limits.MaxCellBytes) {
					if cut == nil {
						cut = map[dbclient.CellAt]bool{}
					}
					cut[dbclient.CellAt{Row: i, Column: j}] = true
				}
			}
			row = values
		}
		tag, err := reader.Close()
		if err == nil {
			err = decodeErr
		}
		if err != nil {
			undo()
			return dbclient.ApplyOutcome{}, &dbclient.ApplyError{Change: i, Of: len(changes), Err: wrapDB(err)}
		}
		if _, insert := change.(dbclient.RowChangeInsert); !insert && tag.RowsAffected() == 0 {
			undo()
			return dbclient.ApplyOutcome{}, &dbclient.ApplyError{Change: i, Of: len(changes), Conflict: true}
		}
		if _, deleted := change.(dbclient.RowChangeDelete); !deleted {
			rows[i] = row
		}
	}

	if inside {
		if err := execAll(ctx, pg, "RELEASE SAVEPOINT "+savepoint); err != nil {
			undo()
			return dbclient.ApplyOutcome{}, wrapDB(err)
		}
		return dbclient.ApplyOutcome{Rows: rows, Cut: cut}, nil
	}
	if err := execAll(ctx, pg, "COMMIT"); err != nil {
		undo()
		return dbclient.ApplyOutcome{}, wrapDB(err)
	}
	return dbclient.ApplyOutcome{Rows: rows, Committed: true, Cut: cut}, nil
}

// PreviewEdit is the statements Apply would send, values as literals.
func (c *PostgresConnection) PreviewEdit(table *dbclient.EditTarget, changes []dbclient.RowChange, returning []int16) []string {
	return previewChanges(table, changes, returning)
}

// Reload reads one row again by its key, down this session, so a row the
// user's own transaction changed reads as they see it.
func (s *PostgresSession) Reload(
	ctx context.Context,
	table *dbclient.EditTarget,
	key []dbclient.AttrValue,
	columns []int16,
) ([]dbclient.Value, error) {
	conn, err := s.held.lock()
	if err != nil {
		return nil, err
	}
	defer s.held.release()
	pg := conn.PgConn()
	if pg.TxStatus() == txFailed {
		return nil, dbclient.ErrTransactionFailed
	}
	names := make([]string, len(columns))
	oids := make([]uint32, len(columns))
	for i, attnum := range columns {
		names[i] = query.QuoteIdent(table.Columns[attnum].Name)
		oids[i] = table.Columns[attnum].TypeOID
	}
	var where []string
	params := make([][]byte, len(key))
	for i, part := range key {
		where = append(where, query.QuoteIdent(table.Columns[part.Column].Name)+" = $"+strconv.Itoa(i+1))
		params[i] = paramText(part.Value)
	}
	sql := fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s", strings.Join(names, ", "),
		query.QuoteIdent(table.Schema), query.QuoteIdent(table.Name), strings.Join(where, " AND "))
	reader := pg.ExecParams(ctx, sql, params, make([]uint32, len(params)), nil, resultFormats(oids))
	var row []dbclient.Value
	for reader.NextRow() {
		if row != nil {
			continue
		}
		fields := reader.FieldDescriptions()
		raw := reader.Values()
		row = make([]dbclient.Value, len(raw))
		for j := range raw {
			value, err := decode(raw[j], fields[j].Format, fields[j].DataTypeOID, j)
			if err != nil {
				_, _ = reader.Close()
				return nil, err
			}
			row[j] = value.Cap(s.limits.MaxCellBytes)
		}
	}
	if _, err := reader.Close(); err != nil {
		return nil, wrapDB(err)
	}
	return row, nil
}

// previewChanges is Preview without a session, for the tests.
func previewChanges(table *dbclient.EditTarget, changes []dbclient.RowChange, returning []int16) []string {
	out := make([]string, 0, len(changes))
	for _, change := range changes {
		out = append(out, buildChange(table, change, returning, true).sql+";")
	}
	return out
}

// execAll runs one bare statement and reads it to the end.
func execAll(ctx context.Context, pg *pgconn.PgConn, sql string) error {
	_, err := pg.Exec(ctx, sql).ReadAll()
	return err
}

// statement is one change as SQL and the values its placeholders name.
type statement struct {
	sql    string
	params []dbclient.Value
}

// buildChange writes one change as SQL. With `literal` the values are
// written into the text, for the user to read; without it they are
// placeholders, and the values go out as parameters.
func buildChange(table *dbclient.EditTarget, change dbclient.RowChange, returning []int16, literal bool) statement {
	var st statement
	arg := func(value dbclient.Value) string {
		if literal {
			return literalOf(value)
		}
		st.params = append(st.params, value)
		return "$" + strconv.Itoa(len(st.params))
	}
	name := func(attnum int16) string {
		return query.QuoteIdent(table.Columns[attnum].Name)
	}
	relation := query.QuoteIdent(table.Schema) + "." + query.QuoteIdent(table.Name)
	where := func(key []dbclient.AttrValue) string {
		parts := make([]string, 0, len(key))
		for _, part := range key {
			parts = append(parts, name(part.Column)+" = "+arg(part.Value))
		}
		return strings.Join(parts, " AND ")
	}
	tail := ""
	if len(returning) > 0 {
		names := make([]string, len(returning))
		for i, attnum := range returning {
			names[i] = name(attnum)
		}
		tail = "\nRETURNING " + strings.Join(names, ", ")
	}

	var b strings.Builder
	switch change := change.(type) {
	case dbclient.RowChangeUpdate:
		sets := make([]string, len(change.Set))
		for i, set := range change.Set {
			sets[i] = name(set.Column) + " = " + arg(set.Value)
		}
		fmt.Fprintf(&b, "UPDATE %s SET %s\n WHERE %s%s", relation, strings.Join(sets, ", "), where(change.Key), tail)
	case dbclient.RowChangeInsert:
		if len(change.Values) == 0 {
			fmt.Fprintf(&b, "INSERT INTO %s DEFAULT VALUES%s", relation, tail)
			break
		}
		columns := make([]string, len(change.Values))
		values := make([]string, len(change.Values))
		for i, value := range change.Values {
			columns[i] = name(value.Column)
			values[i] = arg(value.Value)
		}
		fmt.Fprintf(&b, "INSERT INTO %s (%s)\n VALUES (%s)%s", relation, strings.Join(columns, ", "), strings.Join(values, ", "), tail)
	case dbclient.RowChangeDelete:
		fmt.Fprintf(&b, "DELETE FROM %s\n WHERE %s", relation, where(change.Key))
	}
	st.sql = b.String()
	return st
}

// paramText is a value as the text a parameter carries; nil is NULL.
func paramText(value dbclient.Value) []byte {
	switch v := value.(type) {
	case dbclient.ValueNull:
		return nil
	case dbclient.ValueBool:
		if v {
			return []byte("t")
		}
		return []byte("f")
	case dbclient.ValueInt:
		return []byte(strconv.FormatInt(int64(v), 10))
	case dbclient.ValueFloat:
		return []byte(strconv.FormatFloat(float64(v), 'g', -1, 64))
	case dbclient.ValueBytes:
		return []byte(`\x` + hex.EncodeToString(v))
	case dbclient.ValueText:
		return []byte(v)
	}
	return []byte(value.Display())
}

// literalOf writes a value as a literal, for the preview and the history.
// The server reads it with `standard_conforming_strings` on, which is the
// default since 9.1, so a backslash is only a backslash.
func literalOf(value dbclient.Value) string {
	switch v := value.(type) {
	case dbclient.ValueNull:
		return "NULL"
	case dbclient.ValueBool:
		if v {
			return "true"
		}
		return "false"
	case dbclient.ValueInt, dbclient.ValueFloat:
		return string(paramText(v))
	}
	return "'" + strings.ReplaceAll(string(paramText(value)), "'", "''") + "'"
}
