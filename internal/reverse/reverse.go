package reverse

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/learttytyri/mulligan/internal/change"
)

// Statement returns the SQL that undoes ev.
func Statement(ev change.Event) (string, error) {
	if err := validate(ev); err != nil {
		return "", err
	}
	switch ev.Op {
	case change.Insert:
		return deleteRow(ev)
	case change.Update:
		return updateRow(ev)
	case change.Delete:
		return insertRow(ev)
	}
	return "", fmt.Errorf("reverse: unsupported op %d", ev.Op)
}

// deleteRow undoes an INSERT by removing the row it created.
func deleteRow(ev change.Event) (string, error) {
	where, err := whereClause(ev.Columns, ev.After)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s LIMIT 1;", qualify(ev.Schema, ev.Table), where), nil
}

// updateRow undoes an UPDATE by restoring the before image of every column the
// statement actually changed. Columns it left alone are not rewritten, so a
// concurrent write to an unrelated column survives the reversal.
//
// The row is located by its after-image key, because that is the key the row
// carries in the database right now — including the case where the statement
// changed the key itself.
func updateRow(ev change.Event) (string, error) {
	var sets []string
	skippedReadOnly := false

	for i, c := range ev.Columns {
		if equalValue(ev.Before[i], ev.After[i]) {
			continue
		}
		// A column the database computes rejects any assignment, and restoring
		// what it derives from brings it back on its own.
		if c.ReadOnly {
			skippedReadOnly = true
			continue
		}
		lit, err := literal(ev.Before[i])
		if err != nil {
			return "", err
		}
		sets = append(sets, fmt.Sprintf("%s = %s", quoteIdent(c.Name), lit))
	}
	if len(sets) == 0 {
		if skippedReadOnly {
			return "", fmt.Errorf("reverse: every column the update changed on %s is marked read-only, leaving nothing to restore", qualify(ev.Schema, ev.Table))
		}
		return "", fmt.Errorf("reverse: update changed no columns on %s, nothing to revert", qualify(ev.Schema, ev.Table))
	}
	where, err := whereClause(ev.Columns, ev.After)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s LIMIT 1;",
		qualify(ev.Schema, ev.Table),
		strings.Join(sets, ", "),
		where), nil
}

// equalValue reports whether two column values are identical. reflect is used
// rather than == because binlog values include uncomparable types such as
// []byte for blob and text columns.
func equalValue(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// insertRow undoes a DELETE by re-inserting the row image it removed.
func insertRow(ev change.Event) (string, error) {
	names := make([]string, 0, len(ev.Columns))
	vals := make([]string, 0, len(ev.Columns))
	for i, c := range ev.Columns {
		// Naming a generated column in the column list is rejected outright; the
		// database fills it in from the rest.
		if c.ReadOnly {
			continue
		}
		lit, err := literal(ev.Before[i])
		if err != nil {
			return "", err
		}
		names = append(names, quoteIdent(c.Name))
		vals = append(vals, lit)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("reverse: every column of %s is read-only, leaving nothing to insert", qualify(ev.Schema, ev.Table))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);",
		qualify(ev.Schema, ev.Table),
		strings.Join(names, ", "),
		strings.Join(vals, ", ")), nil
}

// whereClause builds a predicate identifying the single row described by row.
//
// The primary key is used when the table has one. Otherwise the whole row image
// becomes the predicate, which is the only way left to name the row — every
// caller pairs it with LIMIT 1 so identical duplicate rows are not all matched.
func whereClause(cols []change.Column, row []any) (string, error) {
	idx := primaryKeyIndexes(cols)
	if len(idx) == 0 {
		idx = make([]int, len(cols))
		for i := range cols {
			idx[i] = i
		}
	}
	preds := make([]string, len(idx))
	for n, i := range idx {
		name := quoteIdent(cols[i].Name)

		// `col = NULL` is never true, so a NULL has to be compared with IS NULL
		// or the reversal silently matches no rows.
		if row[i] == nil {
			preds[n] = name + " IS NULL"
			continue
		}
		lit, err := literal(row[i])
		if err != nil {
			return "", err
		}
		preds[n] = fmt.Sprintf("%s = %s", name, lit)
	}
	return strings.Join(preds, " AND "), nil
}

// primaryKeyIndexes returns the positions of the primary key columns, in
// declaration order.
func primaryKeyIndexes(cols []change.Column) []int {
	var idx []int
	for i, c := range cols {
		if c.PrimaryKey {
			idx = append(idx, i)
		}
	}
	return idx
}

func qualify(schema, table string) string {
	return quoteIdent(schema) + "." + quoteIdent(table)
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
