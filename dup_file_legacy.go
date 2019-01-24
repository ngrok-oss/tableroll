// +build !go1.12

package shakiin

import (
	"os"
)

func dupFile(fh *os.File, name fileName) (*file, error) {
	return dupFd(fh.Fd(), name)
}
