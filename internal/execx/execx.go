// Package execx is a small command runner abstraction so every external
// dependency (docker, tailscale, sqlite3, sops, secretspec) can be faked in
// tests without touching the host.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Options controls a single command invocation.
type Options struct {
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Result is the captured outcome of a command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner executes a command.
type Runner interface {
	Run(ctx context.Context, name string, args []string, opts Options) (Result, error)
}

// OS runs commands on the real system.
type OS struct{}

// Run implements Runner.
func (OS) Run(ctx context.Context, name string, args []string, opts Options) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = opts.Dir
	cmd.Env = append(cmd.Environ(), opts.Env...)
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}
	var stdout, stderr bytes.Buffer
	if opts.Stdout != nil {
		cmd.Stdout = io.MultiWriter(&stdout, opts.Stdout)
	} else {
		cmd.Stdout = &stdout
	}
	if opts.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&stderr, opts.Stderr)
	} else {
		cmd.Stderr = &stderr
	}

	err := cmd.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		detail := strings.TrimSpace(string(result.Stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(result.Stdout))
		}
		if detail != "" {
			return result, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
		}
		return result, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return result, nil
}

// Call records one fake invocation.
type Call struct {
	Name string
	Args []string
	Opts Options
}

// Fake records calls and delegates to Func.
type Fake struct {
	Calls []Call
	Func  func(call Call) (Result, error)
}

// Run implements Runner.
func (f *Fake) Run(_ context.Context, name string, args []string, opts Options) (Result, error) {
	call := Call{Name: name, Args: append([]string(nil), args...), Opts: opts}
	f.Calls = append(f.Calls, call)
	if f.Func != nil {
		return f.Func(call)
	}
	return Result{}, nil
}

// CommandLines renders the recorded calls for test assertions.
func (f *Fake) CommandLines() []string {
	lines := make([]string, 0, len(f.Calls))
	for _, call := range f.Calls {
		lines = append(lines, strings.TrimSpace(call.Name+" "+strings.Join(call.Args, " ")))
	}
	return lines
}
