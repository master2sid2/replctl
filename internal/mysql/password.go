package mysql

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// Character classes of generated passwords. The special characters need no
// escaping in SQL string literals or inside shell quotes.
var passwordClasses = []string{
	"abcdefghijklmnopqrstuvwxyz",
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"0123456789",
	"-_.,+=:^~",
}

// GeneratePassword returns a random password of n characters (n >= 4) with at
// least one character of every class, so it satisfies the validate_password
// component's MEDIUM and STRONG policies.
func GeneratePassword(n int) string {
	all := strings.Join(passwordClasses, "")
	for {
		b := make([]byte, n)
		for i := range b {
			b[i] = all[randIndex(len(all))]
		}
		if hasEveryClass(string(b)) {
			return string(b)
		}
	}
}

func hasEveryClass(s string) bool {
	for _, class := range passwordClasses {
		if !strings.ContainsAny(s, class) {
			return false
		}
	}
	return true
}

func randIndex(n int) int {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return int(i.Int64())
}
