package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/basetree"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

func init() { register("summary", runSummary) }

// runSummary writes the rules PR summary (Markdown, starting with
// data.SummaryMarker) for head, the working tree of -root, against -base:
// a directory written by basedir, or a git ref of -root from which the same
// set is read. It is informational: a section it cannot compute becomes a
// note, and it exits non-zero only when it cannot read its inputs or write
// the output.
//
// The sections that need the engine (the corpus behaviour diff, the ★ flag,
// compiled rule IDs, overlay hygiene) come from the root module's
// TestSummaryData, run through `go test`, because this module is
// stdlib-only. -validate adds the root `go test -run '^TestRules$'` results.
func runSummary(ctx context.Context, e *Env, args []string) int {
	fs := newFlags(e, "summary")
	root := fs.String("root", "", "linkclean checkout (head is its working tree)")
	base := fs.String("base", "", "base: a basedir output directory, or a git ref of -root")
	upstream := fs.String("upstream", "", "upstream clone made by `rulegen upstream` (default $LINKCLEAN_UPSTREAM_DIR)")
	validate := fs.Bool("validate", false, "run the root TestRules gates and report them (preflight)")
	out := fs.String("out", "", "output file, or - for standard output")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if !requireFlags(fs, "root", "base", "out") {
		return exitUsage
	}
	fail := func(err error) int {
		fmt.Fprintf(e.Stderr, "rulegen summary: %v\n", err)
		return exitFail
	}
	rootDir, err := filepath.Abs(*root)
	if err != nil {
		return fail(err)
	}
	up := *upstream
	if up == "" {
		up = e.Getenv("LINKCLEAN_UPSTREAM_DIR")
	}
	if up != "" {
		if up, err = filepath.Abs(up); err != nil {
			return fail(err)
		}
	}
	tmp, err := os.MkdirTemp("", "rulegen-summary-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)

	r := e.Repo(rootDir)
	baseDir, err := summaryBaseDir(ctx, r, *base, filepath.Join(tmp, "base"))
	if err != nil {
		return fail(err)
	}
	s := &summary{
		e:        e,
		root:     rootDir,
		baseDir:  baseDir,
		upstream: up,
		now:      e.Now(),
		base:     &sumSide{name: "base", fsys: os.DirFS(baseDir)},
		head:     &sumSide{name: "head", fsys: os.DirFS(rootDir)},
	}
	if b := s.base.read(data.GitlinkFile); b != nil {
		s.base.gitlink = strings.TrimSpace(string(b))
	}
	if g, ok, err := r.Gitlink(ctx, "HEAD", data.Submodule); err != nil {
		s.head.notes = append(s.head.notes, fmt.Sprintf("head gitlink: %v", err))
	} else if ok {
		s.head.gitlink = g
	}
	s.base.load()
	s.head.load()
	s.engine, s.engineNote = summaryEngine(ctx, e, rootDir, baseDir, tmp)
	if *validate {
		s.preflight = summaryPreflight(ctx, e, rootDir, baseDir, up)
	}
	s.watch = summaryWatch(ctx, e, rootDir)

	md := s.render(ctx)
	if *out == "-" {
		_, err = e.Stdout.Write(md)
	} else {
		err = summaryWriteFile(*out, md)
	}
	if err != nil {
		return fail(err)
	}
	return exitOK
}

// summaryBaseDir returns base as a directory: base itself when it is one,
// else the output set of the git ref base, written to tmp.
func summaryBaseDir(ctx context.Context, r repo.Repo, base, tmp string) (string, error) {
	if fi, err := os.Stat(base); err == nil && fi.IsDir() {
		return filepath.Abs(base)
	}
	if strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("-base %q is neither a directory nor a ref", base)
	}
	if _, ok, err := r.RevParse(ctx, base+"^{commit}"); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("-base %q is neither a directory nor a commit of %s", base, r.Dir)
	}
	if err := basetree.Write(ctx, r, base, tmp); err != nil {
		return "", err
	}
	return tmp, nil
}

func summaryWriteFile(name string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	return os.WriteFile(name, b, 0o644)
}

// summary is one summary run: its inputs and the sections computed from them.
type summary struct {
	e                *Env
	root, baseDir    string
	upstream         string // "" without a clone
	now              time.Time
	base, head       *sumSide
	engine           *data.SummaryData // nil when TestSummaryData failed
	engineNote       string
	preflight, watch []string // rendered sections
}
