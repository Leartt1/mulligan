package change

import "testing"

// Raw reaches a generated script unquoted, so anything this accepts is executed
// verbatim by whoever runs the script. Every producer and consumer of a Raw
// checks it here; a second copy of this predicate elsewhere would be free to
// drift, and the drift that hurts is silent in both directions — a looser copy
// admits SQL, a stricter one drops changes from the only record of them.
func TestValidRawRejectsAnythingThatIsNotANumber(t *testing.T) {
	hostile := []string{
		"1; DROP TABLE orders",
		"1 OR 1=1",
		"(SELECT 1)",
		"NULL",
		"",
		"1'",
		"0x41",
		"1--",
		"1 ",
		" 1",
		"1\n",
		"--1",
		"1.2.3",
		".5",
		"1.",
		"1e",
		"1e+",
		"e10",
		"+",
		"-",
		"1_000",
		"inf",
		"NaN",
	}

	for _, in := range hostile {
		t.Run(in, func(t *testing.T) {
			if ValidRaw(in) {
				t.Errorf("ValidRaw(%q) = true, want false", in)
			}
		})
	}
}

func TestValidRawAcceptsPlainNumbers(t *testing.T) {
	fine := []string{
		"0",
		"-1",
		"+7",
		"1234.5678",
		"10.00",
		"-0.001",
		"1e10",
		"1.5E-3",
		"1E+9",
		"18446744073709551615",
		"-9223372036854775808",
	}

	for _, in := range fine {
		t.Run(in, func(t *testing.T) {
			if !ValidRaw(in) {
				t.Errorf("ValidRaw(%q) = false, want true", in)
			}
		})
	}
}
