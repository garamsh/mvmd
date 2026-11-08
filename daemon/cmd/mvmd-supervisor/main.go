package main

import (
	"os"

	"github.com/garamsh/mvmd/daemon/internal/supervisor"
)

func main() {
	os.Exit(supervisor.Main(os.Args[1:], os.Stderr))
}
