//go:build !linux

package toolaccess

import "errors"

func makeFIFO(path string, mode uint32) error {
	return errors.New("FIFO fixture is a Linux vector")
}
