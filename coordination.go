package tableroll

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
	"github.com/rkt/rkt/pkg/lock"
)

// errNoOwner indicates that either no process currently is marked as
// controlling the upgradeable file descriptors (e.g. initial startup case), or
// a process is supposed to own them but is dead (e.g. it crashed).
var errNoOwner = errors.New("no owner process exists")

// coordination is used to coordinate between N processes, one of which is the
// current owner.
// It must provide means of getting the owner, updating the owner, and.
// ensuring it has unique ownership of that information for the duration
// between a read and update.
// It is implemented in this case with unix locks on a file.
type coordinator struct {
	lock *lock.FileLock
	dir  string
	l    log15.Logger

	// mocks
	os osIface
}

func touchFile(path string) error {
	_, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0755)
	return err
}

// lockCoordinationDir takes an exclusive lock on the given coordination
// directory. It returns a coordinator that holds the lock and may be used to
// manipulate the directory. If the directory is already locked, the function
// will block until the lock can be acquired.
func lockCoordinationDir(os osIface, l log15.Logger, dir string) (*coordinator, error) {
	l = l.New("dir", dir)
	coord := &coordinator{dir: dir, l: l, os: os}
	pidPath := coord.pidFile()
	if err := touchFile(pidPath); err != nil {
		return nil, err
	}
	l.Info("taking lock on coordination dir", "dir", dir)
	lock, err := lock.ExclusiveLock(pidPath, lock.RegFile)
	if err != nil {
		return nil, err
	}
	l.Info("took lock on coordination dir", "dir", dir)
	coord.lock = lock
	return coord, nil
}

func (c *coordinator) pidFile() string {
	return filepath.Join(c.dir, "pid")
}

func (c *coordinator) BecomeOwner() error {
	pid := c.os.Getpid()
	c.l.Info("writing pid to become owner", "pid", pid)
	return ioutil.WriteFile(c.pidFile(), []byte(strconv.Itoa(pid)), 0755)
}

func (c *coordinator) Unlock() error {
	c.l.Info("unlocking coordination dir")
	return c.lock.Unlock()
}

// GetOwnerPID returns the current 'owner' for this coordination directory.
// It will return '0' as the PID if there is no owner.
func (c *coordinator) GetOwnerPID() (int, error) {
	c.l.Info("discovering current owner")
	data, err := ioutil.ReadFile(c.pidFile())
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		// empty file, that means no owner
		return 0, nil
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("unable to parse pid out of data %q: %v", string(data), err)
	}
	c.l.Info("found owner", "owner", pid)
	return pid, nil
}

func (c *coordinator) ConnectOwner() (*net.UnixConn, error) {
	ppid, err := c.GetOwnerPID()
	if err != nil {
		return nil, err
	}
	c.l.Info("connecting to owner", "owner", ppid)
	if ppid == 0 || pidIsDead(c.os, ppid) {
		c.l.Info("owner does not exist or is dead", "owner", ppid)
		return nil, errNoOwner
	}

	sockPath := upgradeSockPath(c.dir, ppid)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		// Assume this is ECONNREFUSED even though we can't reliably detect it.
		// ECONNREFUSED here means that the pidfile had X in it, process X's pid is
		// alive (possibly due to reuse), and X is not listening on its socket.
		// That means X is a misbehaving tableroll process since it should *never*
		// have let us grabbed the pid lock unless it was also already listening on
		// its sock.  Our best bet is thus to assume nothing about that process and
		// try to take over.
		c.l.Warn("found living pid in coordination dir, but it wasn't listening for us", "pid", ppid, "dialErr", err)
		return nil, errNoOwner
	}
	return conn, nil
}
