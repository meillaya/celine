// Command celine deploys the Calibre and Kavita library stack.
package main

import (
	"os"

	"github.com/Meillaya/celine/internal/cli"
)

// version is overridden by the Nix build with -X main.version.
var version = "0.1.0-dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.App{Version: version}))
}
