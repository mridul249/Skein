package gdrive

import "testing"

// The Drive query is assembled by string concatenation, so a name containing a
// quote would otherwise break out of the literal and change the query. The
// folder name is a constant today; this is the guard for the day it is not.
func TestDriveQuoteEscapes(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Skein", `'Skein'`},
		{"it's", `'it\'s'`},
		{`back\slash`, `'back\\slash'`},
		{`' or trashed = false or '`, `'\' or trashed = false or \''`},
		{"", `''`},
	}
	for _, tc := range tests {
		if got := driveQuote(tc.in); got != tc.want {
			t.Errorf("driveQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// The README has to say what happens, not merely ask nicely — it is the only
// thing between a tidy-minded user and permanent data loss.
func TestReadmeStatesTheConsequence(t *testing.T) {
	for _, want := range []string{"do not delete", "encrypted shards", "permanently destroys"} {
		if !contains(readmeBody, want) {
			t.Errorf("README is missing %q:\n%s", want, readmeBody)
		}
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
