// knock is the command-line client for Knock: open your door to a helper,
// or knock on someone else's, and share one shell through the relay.
package main

import (
	"os"

	"github.com/idestis/knock/cli/internal/app"
)

func main() { os.Exit(app.Main(os.Args[1:])) }
