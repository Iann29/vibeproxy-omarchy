package main

import (
	"strings"
	"testing"
)

func TestSocketInodesForPortFromProcNet(t *testing.T) {
	const procNet = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:207D 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11111 1 0000000000000000 100 0 0 10 0
   1: 0100007F:207E 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 22222 1 0000000000000000 100 0 0 10 0
   2: 0100007F:207D 00000000:0000 01 00000000:00000000 00:00000000 00000000  1000        0 33333 1 0000000000000000 100 0 0 10 0
`

	got, err := socketInodesForPortFromProcNet(strings.NewReader(procNet), 8317)
	if err != nil {
		t.Fatalf("socketInodesForPortFromProcNet returned error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 inode for port 8317, got %d", len(got))
	}

	if _, ok := got["11111"]; !ok {
		t.Fatalf("expected inode 11111 to be returned, got %v", got)
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	got := shellQuote("abc'def")
	want := `'abc'"'"'def'`
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
