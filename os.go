package tableroll

import "os"

type osIface interface {
	Getpid() int
	FindProcess(pid int) (processIface, error)
}

type realOS struct{}

func (realOS) Getpid() int {
	return os.Getpid()
}

func (realOS) FindProcess(pid int) (processIface, error) {
	return os.FindProcess(pid)
}

type processIface interface {
	Signal(os.Signal) error
}
