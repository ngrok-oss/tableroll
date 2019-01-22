package tableroll

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pkg/errors"
	"github.com/rkt/rkt/pkg/lock"
)

// coordination is used to coordinate between N processes, one of which is the
// current parent.
// It must provide means of getting the parent, updating the parent, and
// ensuring it has unique ownership of that information for the duration
// between a read and update.
// It is implemented in this case with unix locks on a file.
type coordinator struct {
	lock *lock.FileLock
	dir  string
}

func touchFile(path string) error {
	_, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0755)
	return err
}

// LockCoordinationDir takes an exclusive lock on the given coordination
// directory. It returns a coordinator that holds the lock and may be used to
// manipulate the directory. If the directory is already locked, the function will block until the lock can be acquired.
func LockCoordinationDir(dir string) (*coordinator, error) {
	coord := &coordinator{dir: dir}
	pidPath := coord.pidFile()
	if err := touchFile(pidPath); err != nil {
		return nil, err
	}
	lock, err := lock.ExclusiveLock(pidPath, lock.RegFile)
	if err != nil {
		return nil, err
	}
	coord.lock = lock
	return coord, nil
}

func (c *coordinator) pidFile() string {
	return filepath.Join(c.dir, "pid")
}

func (c *coordinator) BecomeParent() error {
	return ioutil.WriteFile(c.pidFile(), []byte(strconv.Itoa(os.Getpid())), 0755)
}

func (c *coordinator) Unlock() error {
	return c.lock.Unlock()
}

// GetParentPID returns the current 'parent' for this coordination directory.
// It will return '0' as the PID if there is no parent.
func (c *coordinator) GetParentPID() (int, error) {
	data, err := ioutil.ReadFile(c.pidFile())
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		// empty file, that means no parent
		return 0, nil
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("unable to parse pid out of data %q: %v", string(data), err)
	}
	return pid, nil
}

var ErrNoParent = errors.New("no parent process exists")

func (c *coordinator) ConnectParent() (*net.UnixConn, error) {
	ppid, err := c.GetParentPID()
	if err != nil {
		return nil, err
	}
	if ppid == 0 {
		return nil, ErrNoParent
	}
	if pidIsDead(ppid) {
		return nil, ErrNoParent
	}

	sockPath := upgradeSockPath(c.dir, ppid)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		return nil, errors.Wrap(err, "error connecting to parent")
	}
	return conn, nil
}
