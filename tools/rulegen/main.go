// Command rulegen maintains linkclean's vendored Brave rule data: it clones
// the pinned upstream, syncs the vendored bytes, the lock and the NOTICE
// from the submodule gitlink, writes base inputs and the PR summary for the
// rules gates, and cuts data releases. Run it from the tools module:
//
//	go -C tools run ./rulegen <command> -root .. [flags]
//
// Every path comes from -root; nothing depends on the working directory.
// Exit codes: 0 success, 1 failure, 2 unverifiable (sync -check -content),
// 64 usage error.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

const (
	exitOK           = 0
	exitFail         = 1
	exitUnverifiable = 2
	exitUsage        = 64
)

// Env is everything a command may touch outside its arguments, so tests can
// substitute each part.
type Env struct {
	Stdout, Stderr io.Writer
	Git            repo.Git
	HTTP           *http.Client // summary's watch fetch, release's REST calls
	APIBase        string       // GitHub REST API root, "https://api.github.com"
	Getenv         func(string) string
	Now            func() time.Time
	Sleep          func(context.Context, time.Duration) error
	// Go runs the go command in dir with env added to the process
	// environment.
	Go func(ctx context.Context, dir string, env []string, args ...string) (stdout, stderr []byte, err error)
}

// Repo returns the repository at dir, through e.Git.
func (e *Env) Repo(dir string) repo.Repo { return repo.Repo{Git: e.Git, Dir: dir} }

// command runs one subcommand with the arguments after its name and returns
// the exit code.
type command func(ctx context.Context, e *Env, args []string) int

var commands = map[string]command{}

// register adds a subcommand; each cmd_<name>.go calls it from init.
func register(name string, c command) {
	if _, dup := commands[name]; dup {
		panic("rulegen: command registered twice: " + name)
	}
	commands[name] = c
}

func init() { register("help", runHelp) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, defaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}

// run dispatches args[0] to its command.
func run(ctx context.Context, e *Env, args []string) int {
	if len(args) == 0 {
		usage(e.Stderr)
		return exitUsage
	}
	name := args[0]
	switch name {
	case "-h", "-help", "--help":
		name = "help"
	}
	c, ok := commands[name]
	if !ok {
		fmt.Fprintf(e.Stderr, "rulegen: unknown command %q\n", name)
		usage(e.Stderr)
		return exitUsage
	}
	return c(ctx, e, args[1:])
}

func runHelp(_ context.Context, e *Env, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(e.Stderr, "rulegen help: unexpected arguments %q\n", args)
		return exitUsage
	}
	usage(e.Stdout)
	return exitOK
}

func usage(w io.Writer) {
	var b bytes.Buffer
	b.WriteString("usage: rulegen <command> -root <linkclean checkout> [flags]\n\ncommands:\n")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	b.WriteString("\nRun 'rulegen <command> -h' for its flags.\n")
	w.Write(b.Bytes())
}

// newFlags returns the flag set of a command, reporting to e.Stderr.
func newFlags(e *Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("rulegen "+name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	return fs
}

// parseFlags parses args, which may hold no positional arguments. When ok
// is false the command returns code: 0 after -h, exitUsage otherwise.
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK, false
		}
		return exitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected arguments %q\n", fs.Name(), fs.Args())
		return exitUsage, false
	}
	return exitOK, true
}

// requireFlags reports each named flag left empty and returns false if
// there is one.
func requireFlags(fs *flag.FlagSet, names ...string) bool {
	ok := true
	for _, n := range names {
		if f := fs.Lookup(n); f == nil || f.Value.String() == "" {
			fmt.Fprintf(fs.Output(), "%s: -%s is required\n", fs.Name(), n)
			ok = false
		}
	}
	return ok
}

func defaultEnv() *Env {
	return &Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Git:     repo.Exec{},
		HTTP:    &http.Client{Timeout: time.Minute}, // nil Transport: http.DefaultTransport
		APIBase: "https://api.github.com",
		Getenv:  os.Getenv,
		Now:     time.Now,
		Sleep:   sleep,
		Go:      goCommand,
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func goCommand(ctx context.Context, dir string, env []string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.Bytes(), e.Bytes(), err
}
