// Command entry is the entrypoint of the e2e's bootable image: a static program that prints one
// line and then stays running, so the Machine is Running while the test reads the line.
package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println("hello from the entrypoint")
	time.Sleep(time.Hour)
}
