package change

import "strings"

// ddlPrefixes are the statements that change a table's shape.
//
// Matched on the leading keyword rather than parsed. Getting this wrong in the
// permissive direction means an extra warning on a script; getting it wrong the
// other way means none where one was due, so the list leans towards noticing.
var ddlPrefixes = []string{
	"ALTER ", "CREATE ", "DROP ", "RENAME ", "TRUNCATE ",
}

// IsDDL reports whether a logged statement changes schema rather than rows.
//
// Transaction control and session bookkeeping are explicitly not DDL: they
// surround row changes constantly and warning about them would bury the warnings
// that matter.
func IsDDL(statement string) bool {
	s := strings.ToUpper(strings.TrimSpace(statement))

	switch s {
	case "BEGIN", "COMMIT", "ROLLBACK", "":
		return false
	}

	for _, prefix := range ddlPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
