package main

import (
	"os"

	"github.com/ezke5/pe/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
