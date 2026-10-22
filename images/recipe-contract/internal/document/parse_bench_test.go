package document

import (
	"os"
	"testing"
)

// BenchmarkParseYAML measures Parse only: the source bytes are prepared once and
// no file reading is included. The numbers are measurements for the ADR, never a
// runtime bound on adversarial input.
func BenchmarkParseYAML(b *testing.B) {
	b.Run("Scalar16MiB", func(b *testing.B) {
		source := make([]byte, 0, 16<<20)
		source = append(source, "x: "...)
		for len(source) < 16<<20 {
			source = append(source, 'a')
		}
		source = append(source, '\n')
		b.SetBytes(int64(len(source)))
		for index := 0; index < b.N; index++ {
			if _, err := Parse(source, YAML); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CommittedDshLock", func(b *testing.B) {
		source, err := os.ReadFile("../../../tools/dsh/pnpm-lock.yaml")
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(source)))
		for index := 0; index < b.N; index++ {
			if _, err := Parse(source, YAML); err != nil {
				b.Fatal(err)
			}
		}
	})
}
