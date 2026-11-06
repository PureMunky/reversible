// Command reach reports whether TCP connections to each host:port can be
// opened. It exists so deploy/kind can prove its network rules are
// enforced (busybox wget can't tell "refused by policy" from an HTTP
// error). Exit status: 0 if every address was reached, 1 otherwise.
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	failed := false
	for _, addr := range os.Args[1:] {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			fmt.Printf("unreachable %s (%v)\n", addr, err)
			failed = true
			continue
		}
		c.Close()
		fmt.Printf("reached     %s\n", addr)
	}
	if failed {
		os.Exit(1)
	}
}
