package shakiin

import (
	"os"
	"syscall"
)

var stdEnv = &env{
	newFile:     os.NewFile,
	environ:     os.Environ,
	getenv:      os.Getenv,
	closeOnExec: syscall.CloseOnExec,
}

type env struct {
	newFile     func(fd uintptr, name string) *os.File
	environ     func() []string
	getenv      func(string) string
	closeOnExec func(fd int)
}
