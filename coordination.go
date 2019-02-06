package tableroll

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

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

func newCoordinator(os osIface, l log15.Logger, dir string) *coordinator {
	l = l.New("dir", dir)
	coord := &coordinator{dir: dir, l: l, os: os}
	return coord
}

func (c *coordinator) Listen(ctx context.Context) (*net.UnixListener, error) {
	listenpath := upgradeSockPath(c.dir, c.os.Getpid())
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", listenpath)
	if err != nil {
		return nil, err
	}
	return l.(*net.UnixListener), nil
}

func touchFile(path string) error {
	_, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0755)
	return err
}

// Lock takes an exclusive lock on the given coordination directory.  If the
// directory is already locked, the function will block until the lock can be
// acquired, or until the passed context is cancelled.
func (c *coordinator) Lock(ctx context.Context) error {
	pidPath := c.pidFile()
	if err := touchFile(pidPath); err != nil {
		return err
	}
	c.l.Info("taking lock on coordination dir")
	flock, err := lock.NewLock(pidPath, lock.RegFile)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		err := flock.TryExclusiveLock()
		if err == nil {
			// lock get
			break
		}
		if err != lock.ErrLocked {
			return errors.Wrap(err, "error trying to lock coordination directory")
		}
		// lock busy, wait and try again
		// TODO: mock time for testing speed
		time.Sleep(100 * time.Millisecond)
	}
	c.l.Info("took lock on coordination dir")
	c.lock = flock
	return ctx.Err()
}

func (c *coordinator) pidFile() string {
	return filepath.Join(c.dir, "pid")
}

// BecomeOwner marks this coordinator as the owner of the coordination directory.
// It should only be called while the lock is held.
func (c *coordinator) BecomeOwner() error {
	pid := c.os.Getpid()
	c.l.Info("writing pid to become owner", "pid", pid)
	return ioutil.WriteFile(c.pidFile(), []byte(strconv.Itoa(pid)), 0755)
}

// Unlock unlocks the coordination pid file
func (c *coordinator) Unlock() error {
	if c.lock == nil {
		c.l.Info("not unlocking coordination dir; not locked")
		return nil
	}
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

func (c *coordinator) ConnectOwner(ctx context.Context) (*net.UnixConn, error) {
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
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
	if err != nil {
		if isContextDialErr(err) {
			return nil, err
		}
		// Otherwise assume this is ECONNREFUSED even though we can't reliably
		// detect it.
		// ECONNREFUSED here means that the pidfile had X in it, process X's pid is
		// alive (possibly due to reuse), and X is not listening on its socket.
		// That means X is a misbehaving tableroll process since it should *never*
		// have let us grab the pid lock unless it was also already listening on
		// its socket.  Our best bet is thus to assume that process is not a
		// tableroll process and just take over.
		c.l.Warn("found living pid in coordination dir, but it wasn't listening for us", "pid", ppid, "dialErr", err)
		return nil, errNoOwner
	}
	return conn.(*net.UnixConn), nil
}

func isContextDialErr(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		err = opErr.Err
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

func upgradeSockPath(coordinationDir string, pid int) string {
	return filepath.Join(coordinationDir, fmt.Sprintf("%d.sock", pid))
}
