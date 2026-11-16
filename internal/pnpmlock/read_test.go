package pnpmlock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPinTextDecoder(t *testing.T) {
	for _, test := range []struct {
		name   string
		prefix []byte
		want   string
	}{
		{"BOM", []byte("\xef\xbb\xbf"), ""},
		{"invalid-UTF8", []byte{0xff}, "The encoded data was not valid for encoding utf-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefix := fixture(t, "tiny-dsh")
			path := filepath.Join(string(prefix), pinFile)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(test.prefix, source...), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = ReadProject(prefix, KindDsh)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != test.want {
				t.Fatalf("got %v", err)
			}
		})
	}
}
