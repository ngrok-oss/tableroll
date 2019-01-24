package shakiin

import (
	"fmt"
	"path/filepath"
)

func upgradeSockPath(coordinationDir string, pid int) string {
	return filepath.Join(coordinationDir, fmt.Sprintf("%d.sock", pid))
}
