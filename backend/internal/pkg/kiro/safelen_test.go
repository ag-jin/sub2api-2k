package kiro

import "testing"

func TestThinkingSafeEmitLen(t *testing.T) {
	cases := []struct {
		buf  string
		want int
	}{
		{"hello world", 11},                       // no '<' at all -> emit all
		{"if a < b then", 13},                     // inline '<' not a close-tag prefix -> emit all
		{"reasoning</thinking>", 9},               // full close tag -> hold from tag
		{"reasoning</thi", len("reasoning")},      // partial prefix at end -> hold the prefix
		{"reasoning</", len("reasoning")},         // shorter partial prefix
		{"done<", 4},                              // '<' is prefix of '</...' -> hold it
	}
	for _, c := range cases {
		if got := thinkingSafeEmitLen(c.buf); got != c.want {
			t.Errorf("thinkingSafeEmitLen(%q) = %d, want %d", c.buf, got, c.want)
		}
	}
}
