package main

import (
	"os"

	"github.com/garamsh/mvmd/daemon/internal/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:], os.Stderr))
}
