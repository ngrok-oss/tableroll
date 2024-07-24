//go:build !go1.12
// +build !go1.12

package tableroll

import (
	"os"
)

func dupFile(fh *os.File, name string) (*file, error) {
	return dupFd(fh.Fd(), name)
}
