// Package tempname builds the random temporary names the retired Node helpers
// took from crypto.randomUUID().
package tempname

import (
	"crypto/rand"
	"fmt"
)

// UUID4 returns a random version-4 UUID in its canonical lowercase text form.
func UUID4() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}
