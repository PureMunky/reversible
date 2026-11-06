// Command runner executes generated services for the factory. In kind it
// runs in its own container beside the factory, as a native sidecar: a
// separate process namespace, no secrets, no credentials. The factory
// (with REVERSIBLE_RUNNER=http://127.0.0.1:7070) builds the code and asks
// the runner to start and stop it; model-written code never runs where it
// could read the factory's environment or memory.
//
//	runner -addr 127.0.0.1:7070 -root /shared
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PureMunky/reversible/factory"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7070", "listen address (localhost: only the pod's containers can reach it)")
	root := flag.String("root", "/shared", "only binaries and data under this directory may be run")
	flag.Parse()
	h, stopAll := factory.RunnerHandler(*root)
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stopAll()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("runner: executing generated services from %s, on %s", *root, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
