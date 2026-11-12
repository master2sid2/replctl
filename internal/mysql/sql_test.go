package mysql

import "testing"

func TestQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", `'plain'`},
		{"it's", `'it''s'`},
		{`back\slash`, `'back\\slash'`},
		{`'; DROP USER x; --`, `'''; DROP USER x; --'`},
	}
	for _, tt := range tests {
		if got := quote(tt.in); got != tt.want {
			t.Errorf("quote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestMaskSQL(t *testing.T) {
	stmts := func(pass string) []string {
		return []string{
			"CREATE USER " + account("u", "10.0.0.1") + " " + identifiedBy(pass, false),
			"CREATE USER " + account("u", "10.0.0.1") + " " + identifiedBy(pass, true),
			"CLONE INSTANCE FROM 'clone_user'@'10.0.0.1':3306 IDENTIFIED BY " + quote(pass),
			"CHANGE REPLICATION SOURCE TO SOURCE_USER='r', SOURCE_PASSWORD=" + quote(pass) + ", SOURCE_AUTO_POSITION=1",
		}
	}
	// A masked statement must not depend on the password at all.
	want := stmts("x")
	for i := range want {
		want[i] = maskSQL(want[i])
	}
	for _, pass := range []string{"secret", "it's", `back\slash`, `a'b\'c`, `ends\`, "", "BY 'x'"} {
		for i, s := range stmts(pass) {
			if got := maskSQL(s); got != want[i] {
				t.Errorf("password %q leaks: %s", pass, got)
			}
		}
	}
}
