package toolchain

import "fmt"

// Refusal is a pin, manifest, trust or installed-state rule this layer will not
// act on. The message is the contract: the shell suites and the differential
// oracle match diagnostic fragments, and after "rust_toolchain: " it is the
// source reader's text.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

// refuse builds a Refusal.
func refuse(format string, arguments ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, arguments...)}
}

// adopt turns another package's refusal (the strict JSON decoder's, the gpg
// verifier's) into this package's, keeping its message byte for byte. One
// refusal type is what lets a test assert that a hole in the grammar is a
// refusal rather than a stray internal error.
func adopt(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*Refusal); ok {
		return err
	}
	return &Refusal{Message: err.Error()}
}
