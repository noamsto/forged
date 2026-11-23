package main

import (
	"os"

	"github.com/noamsto/forged/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
