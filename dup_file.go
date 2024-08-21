//go:build go1.12
// +build go1.12

package tableroll

import (
	"os"
)

func dupFile(fh *os.File, name string) (*file, error) {
	// os.File implements syscall.Conn from go 1.12
	return dupConn(fh, name)
}
