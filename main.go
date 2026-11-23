package main

import (
	"os"

	"github.com/noamsto/forge-cache/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
