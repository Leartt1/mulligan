package change

import "testing"

func TestIsDDLRecognisesStatementsThatChangeATablesShape(t *testing.T) {
	ddl := []string{
		"ALTER TABLE orders ADD COLUMN note VARCHAR(32)",
		"alter table orders drop column note",
		"  ALTER TABLE orders MODIFY COLUMN amount VARCHAR(16)",
		"CREATE TABLE orders (id INT)",
		"DROP TABLE orders",
		"RENAME TABLE orders TO old_orders",
		"TRUNCATE TABLE orders",
		"CREATE INDEX ix ON orders (id)",
	}

	for _, in := range ddl {
		t.Run(in, func(t *testing.T) {
			if !IsDDL(in) {
				t.Errorf("IsDDL(%q) = false, want true", in)
			}
		})
	}
}

// Transaction control surrounds row changes constantly. Treating it as a schema
// change would put a warning on every script and bury the ones that matter.
func TestIsDDLIgnoresTransactionControlAndRowStatements(t *testing.T) {
	notDDL := []string{
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
		"begin",
		"",
		"   ",
		"INSERT INTO orders VALUES (1)",
		"UPDATE orders SET status = 'shipped'",
		"DELETE FROM orders WHERE id = 1",
		"SELECT 1",
		"SET NAMES utf8mb4",
	}

	for _, in := range notDDL {
		t.Run(in, func(t *testing.T) {
			if IsDDL(in) {
				t.Errorf("IsDDL(%q) = true, want false", in)
			}
		})
	}
}
