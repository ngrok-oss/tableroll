# tableroll &mdash; Coordinate upgrades between processes

tableroll is a graceful upgrade process for network services. It allows
zero-downtime upgrades (such that the listening socket never drops a
connection) between multiple [Go](https://golang.org/) processes.

It is inspired heavily by cloudflare's
[tableflip](https://github.com/cloudflare/tableflip) library.
The primary difference between 'tableflip' and 'tableroll' is that 'tableroll'
does not require updates to re-use the existing executable binary nor does it
enforce any process heirarchy between the old and new processes.

It is expected that the old and new processes in a tableroll upgrade will both
be managed by an external service manager, such as a systemd template unit.

Instead of coordinating upgrades between a parent and child process, tableroll
coordinates upgrades between a number of processes that agree on a well-known
filesystem path ahead of time, and which all have access to that path.

## Usage

tableroll's usage is similar to [tableflip](https://github.com/cloudflare/tableflip)'s usage.

In general, your process should do the following:

1. Construct an upgrader using `tableroll.New`
1. Create or add all managed listeners / connections / files via `upgrader.Fds`.
1. Mark itself as ready to accept connections using `upgrader.Ready`
1. Wait for a request to exit using the `upgrader.UpgradeComplete` channel
1. Close all managed listeners and drain all connections (e.g. using `server.Shutdown` on `http.Server`)

One example usage might be the following:

### Usage Example

The following example shows a simple usage of tableroll.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/ngrok/tableroll"
)

func main() {
	ctx := context.Background()
	logger := log15.New()
	if err := os.MkdirAll("/tmp/testroll", 0700); err != nil {
		log.Fatalf("can't create coordination dir: %v", err)
	}
	tablerollID := strconv.Itoa(os.Getpid())
	upg, err := tableroll.New(ctx, "/tmp/testroll", tablerollID, tableroll.WithLogger(logger))
	if err != nil {
		panic(err)
	}
	ln, err := upg.Fds.Listen(ctx, "port-8080", &net.ListenConfig{}, "tcp", "127.0.0.1:8080")
	if err != nil {
		log.Fatalf("can't listen: %v", err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(r http.ResponseWriter, req *http.Request) {
			logger.Info("got http connection")
			time.Sleep(10 * time.Second)
			r.Write([]byte(fmt.Sprintf("hello from %v!\n", os.Getpid())))
		}),
	}
	go server.Serve(ln)

	if err := upg.Ready(); err != nil {
		panic(err)
	}
	<-upg.UpgradeComplete()

	time.AfterFunc(30*time.Second, func() {
		os.Exit(1)
	})

	_ = server.Shutdown(context.Background())

	logger.Info("server shutdown")
}
```

When this program is run, it will listen on port `8080` for http connections.
If you start another copy of it, the newer copy will take over. If you have
pending http requests in-flight, they'll be handled by the old process before
it shuts down.
