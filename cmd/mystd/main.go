// Command mystd is retained as a compatibility entry point. Release builds use
// the same daemon implementation through `myst --daemon`.
package main

import (
	"fmt"
	"os"

	"github.com/zamirszn/mysterium-linux-client/internal/daemon"
)

func main() {
	if err := daemon.RunServer(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mystd:", err)
		os.Exit(1)
	}
}
