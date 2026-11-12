//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/textrender"
)

func main() {
	os.Clearenv()
	syscall.Umask(0077)
	if syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}) != nil {
		os.Exit(textrender.RelayExitFailure)
	}
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(256 << 20)
	input := pollablePipe(0, "renderer-input", true)
	output := pollablePipe(1, "renderer-output", false)
	if input == nil || output == nil {
		os.Exit(textrender.RelayExitFailure)
	}
	// Inherited standard files were initialized as blocking descriptors. Only
	// the explicit nonblocking duplicates participate in Go's runtime poller,
	// so closing input can interrupt and join the liveness reader on success.
	_ = os.Stdin.Close()
	_ = os.Stdout.Close()
	if output.SetWriteDeadline(time.Now().Add(30*time.Second)) != nil {
		os.Exit(textrender.RelayExitFailure)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	stopOutput := context.AfterFunc(ctx, func() { _ = output.SetWriteDeadline(time.Now()) })
	code := textrender.ServeRelay(ctx, input, output)
	stopOutput()
	_ = input.Close()
	_ = output.Close()
	os.Exit(code)
}

func pollablePipe(original int, name string, input bool) *os.File {
	duplicate, err := syscall.Dup(original)
	if err != nil {
		return nil
	}
	syscall.CloseOnExec(duplicate)
	if syscall.SetNonblock(duplicate, true) != nil {
		_ = syscall.Close(duplicate)
		return nil
	}
	file := os.NewFile(uintptr(duplicate), name)
	if file == nil {
		_ = syscall.Close(duplicate)
		return nil
	}
	if input {
		err = file.SetReadDeadline(time.Time{})
	} else {
		err = file.SetWriteDeadline(time.Time{})
	}
	if err != nil {
		_ = file.Close()
		return nil
	}
	return file
}
