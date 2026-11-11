// Command entry is the entrypoint of the e2e's bootable image. Run without arguments it prints one
// line and stays running, so the Machine is Running while the test reads the line. Run as a command
// of an exec, or as arguments of the image's entrypoint, it does one short thing: "echo WORDS..."
// prints them, "exit N" ends with code N, "cat FILE" prints the file, "ls DIR" prints the names in
// the directory, one to a line, "put FILE WORDS..." writes them to the start of the file and syncs it,
// "head FILE N" prints its first N bytes, and "console WORDS..."
// writes them to the guest's console.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
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
	case "ls":
		entries, err := os.ReadDir(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		for _, entry := range entries {
			fmt.Println(entry.Name())
		}
	case "put":
		file, err := os.OpenFile(args[0], os.O_WRONLY, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err := file.WriteString(strings.Join(args[1:], " ")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		// A kernel setting under /proc has nothing to sync and refuses it as invalid.
		if err := file.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		file.Close()
	case "head":
		size, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		file, err := os.Open(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		buf := make([]byte, size)
		n, err := io.ReadFull(file, buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Stdout.Write(buf[:n])
		file.Close()
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
