package kpirank

import (
	"io"
	"testing"
)

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := openRowStore(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	rows := [][]string{
		{"alice", "apple", "10"},
		{"", "", ""},
		{"bob", "x", "-5"},
	}
	for _, r := range rows {
		if err := s.appendRow(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	rd, err := s.openRowReader()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.close()
	i := 0
	for {
		row, err := rd.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		for j := range row {
			if row[j] != rows[i][j] {
				t.Fatalf("row %d col %d: got %q want %q", i, j, row[j], rows[i][j])
			}
		}
		i++
	}
	if i != len(rows) {
		t.Fatalf("got %d rows, want %d", i, len(rows))
	}
}
