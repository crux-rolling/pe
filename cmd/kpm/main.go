package main

import (
	"os"

	"github.com/kryx-linux/kpm/internal/cli"
)

func main() {
	os.Exit(cli.RunWithArgv0(os.Args[0], os.Args[1:], os.Stdout, os.Stderr))
}
