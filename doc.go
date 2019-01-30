// Package tableroll implements zero downtime upgrades between two independently
// managed processes.
//
// An upgrade is coordinated over a well-known coordination directory. Any
// number of processes may be run at once that coordinate upgrades on that
// directory, and between those many processes, one is chosen to own all
// shareable / upgradeable file descriptors.
// Each upgrade uniquely involves two processes, and unix exclusive locks on
// the filesystem ensure that.
//
// Each process under tableroll should be able to signal readiness, which will
// indicate to tableroll that it is safe for previous processes to cease
// listening for new connections and begin draining existing ones.
//
// Unlike other upgrade mechanisms in this space, it is expected that a new
// binary is started independently, such as in a new container, not as a child
// of the existing one. How a new upgrade is started is entirely out of scope
// of this library. Both copies of the process must have access to the same
// coordination directory, but apart from that, there are no stringent
// requirements.
package tableroll
