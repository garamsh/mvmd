// Command entry is the entrypoint of the e2e's bootable image. Run without arguments it prints one
// line and stays running, so the Machine is Running while the test reads the line. Run as a command
// of an exec, or as arguments of the image's entrypoint, it does one short thing: "echo WORDS..."
// prints them, "exit N" ends with code N, "cat FILE" prints the file, "ls DIR" prints the names in
// the directory, one to a line, "put FILE WORDS..." writes them to the start of the file and syncs it,
// "head FILE N" prints its first N bytes, "now" prints the guest's wall clock in nanoseconds since
// the epoch, "console WORDS..." writes them to the guest's console, "dial HOST:PORT" opens a TCP
// connection and prints connected, refused or timeout, and "listen PORT..." accepts TCP connections
// on each port, prints "listening PORT" once each is bound, and stays running, "uname" prints the
// release and the version of the kernel the guest booted, one to a line, and "ignore-term" ignores
// SIGTERM, prints "ignoring SIGTERM" once it does, and stays running.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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
	case "now":
		fmt.Println(time.Now().UnixNano())
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
	case "uname":
		var name syscall.Utsname
		if err := syscall.Uname(&name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(utsField(name.Release[:]))
		fmt.Println(utsField(name.Version[:]))
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ignoring SIGTERM")
		time.Sleep(time.Hour)
	case "dial":
		dial(args[0])
	case "listen":
		listen(args)
	default:
		fmt.Fprintln(os.Stderr, "unknown command", name)
		os.Exit(2)
	}
}

// utsField is the text of a NUL-terminated field of the kernel's utsname.
func utsField(field []int8) string {
	var text []byte
	for _, c := range field {
		if c == 0 {
			break
		}
		text = append(text, byte(c))
	}
	return string(text)
}

// dial prints how a TCP connection to the address ended: connected, refused when the peer answered
// with a reset, timeout when nothing answered, or the error otherwise.
func dial(address string) {
	conn, err := net.DialTimeout("tcp", address, 4*time.Second)
	var timeout net.Error
	switch {
	case err == nil:
		conn.Close()
		fmt.Println("connected")
	case errors.Is(err, syscall.ECONNREFUSED):
		fmt.Println("refused")
	case errors.As(err, &timeout) && timeout.Timeout():
		fmt.Println("timeout")
	default:
		fmt.Println("error", err)
	}
}

// listen binds every port, prints one line for each, then accepts and closes connections for good.
func listen(ports []string) {
	var wg sync.WaitGroup
	for _, port := range ports {
		listener, err := net.Listen("tcp", ":"+port)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("listening", port)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
	}
	wg.Wait()
}
