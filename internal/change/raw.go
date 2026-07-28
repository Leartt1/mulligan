package change

// ValidRaw reports whether s is a plain SQL number: an optional sign, digits, an
// optional fraction, and an optional exponent, with nothing else around it.
//
// Raw is the one value that reaches a generated script without quoting, which
// makes it the only place a source adapter could put text straight into a
// statement an operator runs by hand. Everything that produces or accepts a Raw
// checks it here rather than keeping its own copy: two hand-maintained copies of
// a security predicate drift, and the direction that drifts silently — a producer
// stricter than the renderer — drops changes from the only surviving record of
// them.
//
// Trailing whitespace is rejected along with everything else, since a value that
// needs trimming did not come from a number formatter and should be looked at.
func ValidRaw(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}

	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return false
	}

	if i < len(s) && s[i] == '.' {
		i++
		start = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}

	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}

	return i == len(s)
}
