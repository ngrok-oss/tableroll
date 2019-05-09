// Package proto encapsulates the types used to communicate between multiple
// tableroll processes at various versions, as well as the functions for
// reading and writing this data off the wire.
//
// Currently, there are two protocol versions: v0 and v1.
// The v1 protocol exists because the v0 protocol allows for a new process to
// think it had notified the previous owner it was ready, even if the new owner
// never read that byte.
// The primary reaon this is possible is because the protocol is over a stream
// oriented unix socket, not a unix dgram socket.
// In order to fix this issue, the v1 protocol includes a more complete
// 'upgrade complete' handshake which ensures both processes actively signal
// intent for the new process to take over.
//
// The v1 ready handshake between N, a new process which is attempting to
// become an owner, and O, the current owner / old process, is the following:
//
// N sends 'V1StartReadyHandshake' to O
// N sends 'VersionInformation{Version: 1}' to O
// O sends 'Message{Msg: V1MessageSteppingDown}' to N
// O closes the connection
//
// There are two failure modes of interest here.
// 1. O sends 'SteppingDown' but 'N' doesn't read it.
// 2. O gets an error sending 'SteppingDown' and is unsure if 'N' received it.
//
// In the case of 1. the failure mode is that there is now no owner. The
// expected behavior is that 'N', if it fails to complete the handshake, will
// return an error from 'Ready()' and exit, and will then restart and get
// succeed since there will now be no owner.
// In the case of 2, O must assume that the message sent successfully and
// actually step down. This again leaves us with 0 owners in the failure mode,
// which is what we want.
// All other cases should result in O remaining the owner, or the ownership
// transfer completing successfully.
package proto
