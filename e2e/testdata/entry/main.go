// Command entry is the entrypoint of the e2e's bootable image. Run without arguments it prints one
// line and stays running, so the Machine is Running while the test reads the line. Run as a command
// of an exec, or as arguments of the image's entrypoint, it does one short thing: "echo WORDS..."
// prints them, "exit N" ends with code N, "cat FILE" prints the file, and "console WORDS..."
// writes them to the guest's console.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		command(os.Args[1], os.Args[2:])
		return
	}
	fmt.Println("hello from the entrypoint")
	time.Sleep(time.Hour)
}

func command(name string, args []string) {
	switch name {
	case "echo":
		fmt.Println(strings.Join(args, " "))
	case "exit":
		code, err := strconv.Atoi(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(code)
	case "cat":
		content, err := os.ReadFile(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Stdout.Write(content)
	case "console":
		console, err := os.OpenFile("/dev/console", os.O_WRONLY, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(console, strings.Join(args, " "))
		console.Close()
	default:
		fmt.Fprintln(os.Stderr, "unknown command", name)
		os.Exit(2)
	}
}
