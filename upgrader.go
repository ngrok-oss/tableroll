package tableroll

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
	"k8s.io/utils/clock"
)

// DefaultUpgradeTimeout is the duration in which the upgrader expects the
// sibling to send a 'Ready' notification after passing over all its file
// descriptors; If the sibling does not send that it is ready in that duration,
// this Upgrader will close the sibling's connection and wait for additional connections.
const DefaultUpgradeTimeout time.Duration = time.Minute

// Upgrader handles zero downtime upgrades and passing files between processes.
type Upgrader struct {
	upgradeTimeout time.Duration

	coord       *coordinator
	session     *upgradeSession
	upgradeSock *net.UnixListener
	stopOnce    sync.Once

	stateLock sync.Mutex
	state     upgraderState

	// upgradeCompleteC is closed when this upgrader has serviced an upgrade and
	// is no longer the owner of its Fds.
	// This also occurs when `Stop` is called.
	upgradeCompleteC chan struct{}

	l log15.Logger

	Fds *Fds

	// mocks
	clock clock.Clock
}

// Option is an option function for Upgrader.
// See Rob Pike's post on the topic for more information on this pattern:
// https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html
type Option func(u *Upgrader)

// WithUpgradeTimeout allows configuring the update timeout. If a time of 0 is
// specified, the default will be used.
func WithUpgradeTimeout(t time.Duration) Option {
	return func(u *Upgrader) {
		u.upgradeTimeout = t
		if u.upgradeTimeout <= 0 {
			u.upgradeTimeout = DefaultUpgradeTimeout
		}
	}
}

// WithLogger configures the logger to use for tableroll operations.
// By default, nothing will be logged.
func WithLogger(l log15.Logger) Option {
	return func(u *Upgrader) {
		u.l = l
	}
}

// New constructs a tableroll upgrader.
// The first argument is a directory. All processes in an upgrade chain must
// use the same coordination directory. The provided directory must exist and
// be writeable by the process using tableroll.
// Canonically, this directory is `/run/${program}/tableroll/`.
// The next argument is an 'id', which must be unique per tableroll process.
// This is any opaque string which uniquely identifies this process, such as
// the PID. The identifier will also be used in tableroll log messages.
// Any number of options to configure tableroll may also be provided.
// If the passed in context is cancelled, any attempt to connect to an existing
// owner will be cancelled.  To stop servicing upgrade requests and complete
// stop the upgrader, the `Stop` method should be called.
func New(ctx context.Context, coordinationDir string, id string, opts ...Option) (*Upgrader, error) {
	return newUpgrader(ctx, clock.RealClock{}, coordinationDir, id, opts...)
}

func newUpgrader(ctx context.Context, clock clock.Clock, coordinationDir string, id string, opts ...Option) (*Upgrader, error) {
	noopLogger := log15.New()
	noopLogger.SetHandler(log15.DiscardHandler())
	u := &Upgrader{
		upgradeTimeout:   DefaultUpgradeTimeout,
		state:            upgraderStateCheckingOwner,
		upgradeCompleteC: make(chan struct{}),
		l:                noopLogger,
		clock:            clock,
	}
	for _, opt := range opts {
		opt(u)
	}
	u.coord = newCoordinator(clock, u.l, coordinationDir, id)

	listener, err := u.coord.Listen(ctx)
	if err != nil {
		return nil, err
	}
	u.upgradeSock = listener
	go u.serveUpgrades()

	_, err = u.becomeOwner(ctx)

	return u, err
}

// BecomeOwner upgrades the calling process to the 'owner' of all file descriptors.
// It returns 'true' if it coordinated taking ownership from a previous,
// existing owner process.
// It returns 'false' if it has taken ownership by identifying that no other
// owner existed.
func (u *Upgrader) becomeOwner(ctx context.Context) (bool, error) {
	sess, err := connectToCurrentOwner(ctx, u.l, u.coord)
	if err != nil {
		return false, err
	}
	u.session = sess
	files, err := sess.getFiles(ctx)
	if err != nil {
		sess.Close()
		return false, err
	}
	u.Fds = newFds(u.l, files)
	return sess.hasOwner(), nil
}

func (u *Upgrader) serveUpgrades() {
	for {
		conn, err := u.upgradeSock.AcceptUnix()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				u.l.Info("upgrade socket closed, no longer listening for upgrades")
				return
			}
			u.l.Error("error awaiting upgrade", "err", err)
			continue
		}
		go u.handleUpgradeRequest(conn)
	}
}

func (u *Upgrader) transitionTo(state upgraderState) error {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()
	return u.state.transitionTo(state)
}

func (u *Upgrader) mustTransitionTo(state upgraderState) {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()
	if err := u.state.transitionTo(state); err != nil {
		panic(fmt.Sprintf("BUG: error transitioning to %q: %v", state, err))
	}
}

func (u *Upgrader) handleUpgradeRequest(conn *net.UnixConn) {
	defer func() {
		if err := conn.Close(); err != nil {
			u.l.Warn("error closing connection", "err", err)
		}
		u.l.Debug("closed upgrade socket connection")
	}()

	if err := u.transitionTo(upgraderStateTransferringOwnership); err != nil {
		u.l.Info("cannot handle upgrade request", "reason", err)
		return
	}

	u.l.Info("handling an upgrade request from peer")
	u.Fds.lockMutations(ErrUpgradeInProgress)

	readyTimeout := u.clock.NewTimer(u.upgradeTimeout)
	defer readyTimeout.Stop()
	nextOwner := newSibling(u.l, conn)
	err := nextOwner.giveFDs(readyTimeout.C(), u.Fds.copy())
	if err != nil {
		u.l.Error("failed to pass file descriptors to next owner", "reason", "error", "err", err)
		// remain owner
		if err := u.transitionTo(upgraderStateOwner); err != nil {
			// could happen if 'Stop' was called after 'handleUpgradeRequest'
			// started, and then the request failed.
			// This leaves us in the state of being the sole owner of Fds, but not
			// being able to pass on ownership because that's what 'Stop' indicates
			// is desired.
			// At this point, we can't really do anything but complain.
			u.l.Error("unable to remain owner after upgrade failure", "err", err)
			return
		}
		u.Fds.unlockMutations()
		return
	}

	u.l.Info("next owner is ready, marking ourselves as up for exit")
	// ignore error, if we were 'Stopped' we can't transition, but we also
	// don't care.
	u.Fds.lockMutations(ErrUpgradeCompleted)
	_ = u.transitionTo(upgraderStateDraining)
	close(u.upgradeCompleteC)
}

// Ready signals that the current process is ready to accept connections.
// It must be called to finish the upgrade.
//
// All fds which were inherited but not used are closed after the call to Ready.
func (u *Upgrader) Ready() error {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()

	if err := u.state.canTransitionTo(upgraderStateOwner); err != nil {
		return errors.Errorf("cannot become ready: %v", err)
	}

	defer func() {
		// unlock the coordination dir even if we fail to become the owner, this
		// gives another process a chance at it even if our caller for some
		// reason decides to not panic/exit
		if err := u.session.Close(); err != nil {
			u.l.Error("error closing upgrade session", "err", err)
		}
	}()
	if u.session.hasOwner() {
		// We have to notify the owner we're ready if they exist.
		if err := u.session.readyHandshake(); err != nil {
			return err
		}
	}
	if err := u.session.BecomeOwner(); err != nil {
		return err
	}
	// if we notified the owner without error, or one didn't exist, we're the owner now
	if err := u.state.transitionTo(upgraderStateOwner); err != nil {
		return err
	}
	return nil
}

// UpgradeComplete returns a channel which is closed when the managed file
// descriptors have been passed to the next process, and the next process has
// indicated it is ready.
func (u *Upgrader) UpgradeComplete() <-chan struct{} {
	return u.upgradeCompleteC
}

// Stop prevents any more upgrades from happening, and closes
// the upgrade complete channel.
func (u *Upgrader) Stop() {
	u.mustTransitionTo(upgraderStateStopped)
	if u.session != nil {
		u.session.Close()
	}
	u.stopOnce.Do(func() {
		u.Fds.lockMutations(ErrUpgraderStopped)
		// Interrupt any running Upgrade(), and
		// prevent new upgrade from happening.
		u.upgradeSock.Close()
		select {
		case <-u.upgradeCompleteC:
		default:
			close(u.upgradeCompleteC)
		}
	})
}
