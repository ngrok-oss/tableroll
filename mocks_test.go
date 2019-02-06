package tableroll

import "os"

type mockOS struct {
	pid int
}

func (m mockOS) Getpid() int {
	return m.pid
}

func (m mockOS) FindProcess(pid int) (processIface, error) {
	return mockProcess{nil}, nil
}

type mockProcess struct {
	err error
}

func (m mockProcess) Signal(s os.Signal) error {
	return m.err
}
