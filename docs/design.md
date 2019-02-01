## Tableroll Design

### File descriptor handoff protocol

tableroll defines a custom protocol in order to coordinate taking ownership of a collection of file descriptors over a unix socket. It consists of the following notable filesystem paths:

* The coordination directory &mdash; this directory, typically in
  `/run/${programName}/tableroll`, is used to coordinate state between each
  running process involved in the tableroll upgrade group.
* The pid file &mdash; a `pid` file within the coordination directory is used
  to indicate the current active holder of all file descriptors. A process will
  write its own pid to that file after a handoff with the previous process
  whose pid was in the pid file.
* Coordination unix sockets &mdash; each process will also listen on a socket
  within the coordination directory. This socket is the means by which a file
  descriptor handoff may be initiated, and the medium over which file
  descriptors will be passed. Each socket is named `${pid}.sock` within the
  coordination directory.

#### Handoff protocol

The actual protocol always takes place between two processes. For the sake of
example, let's assume the chosen coordination directory is
`/run/example/tableroll`, and that two processes using that directory exist.
These processes will be the "first" and the "second" processes, where the
"first" process arbitrarily holds two open file descriptors that are marked
passable, '127.0.0.1:80' and '127.0.0.1:443'.

The handoff of these two FDs from "first" to "second" will look like the following:

1. "second" begins listening on `/run/example/tableroll/${second_pid}.sock`.
1. "second" takes an exclusive lock on the pid file, `/run/example/tableroll/pid`.
1. "second" reads the value `${first_pid}` from the pid file.
1. "second" opens a unix connection to `/run/example/tableroll/${first_pid}.sock`.
1. "first" accepts the connection and writes the following data to it:
    1. `70` &mdash; The length of the following message, as a 4 byte signed integer in big endian.
    1. `[["listener","tcp","127.0.0.1:80"],["listener","tcp","127.0.0.1:443"]]` &mdash; A JSON encoding of the names of all file descriptors to expect
    1. *file descriptors* &mdash; Both file descriptors mentioned in the previous
       message, in the same order. These are written using unix's "sendmsg"
       syscall.
1. "second", after reading all the preceeding data, returns control to the
   caller of this library temporarily. It is expected for the caller to do
   necessary work to allow accepting connections on the transfered listening
   sockets.  At this point, both "first" and "second" have functioning
   listening sockets, "second" continues to hold the pid file lock, and
   "second" still has a unix connection open to "first".
1. "second" has its `upgrader.Ready()` method called, which results in the following:
    1. The byte `42` is sent on the open unix connection to "first"
    1. "second" writes its pid to the pid file.
    1. "second" unlocks the exclusive lock it held on the pid file.
    1. "second" closes the unix connection to "first".
1. "first" reads `42` from the unix connection.
1. "first" writes to the 'Exit' channel, indicating to the library user that
   listeners should be closed and connections drained.
