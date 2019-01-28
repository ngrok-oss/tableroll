# tableroll &mdash; Coordinate upgrades between processes

tableroll is a graceful upgrade process for network services. It allows
zero-downtime upgrades (such that the listening socket never drops a
connection) between multiple go processes.

It is inspired heavily by cloudflare's
[tableflip](https://github.com/cloudflare/tableflip) library.
The primary difference between 'tableflip' and 'tableroll' is that 'tableroll'
does not require updates to re-use the existing executable binary nor does it
enforce any process heirarchy between the old and new processes.

It is expected that the old and new procsses in a tableroll upgrade will both
be managed by an external service manager, such as a systemd template unit.

Instead of coordinating upgrades between a parent and child process, tableroll
coordinates upgrades between a number of processes that agree on a well-known
filesystem path ahead of time, and which all have access to that path.
