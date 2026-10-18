// Command discordharness sends discordlink's harness payloads to one private
// Discord channel through the REST API (v10), records what Discord returns,
// and writes a table for the observations only a person can make. It is the
// sending half of the private-channel harness; the payloads come from
//
//	go test ./discordlink -run '^TestHarnessPayloads$' -harness-out "$DIR"
//
// Run it from the tools module, from a terminal (it asks on /dev/tty):
//
//	go -C tools run ./discordharness -payloads "$DIR/payloads.json" \
//	    -token-file <file> -guild <id> -channel <id> -phase all -out "$DIR"
//	go -C tools run ./discordharness -cleanup -token-file <file> \
//	    -guild <id> -channel <id> -out "$DIR"
//
// Safety rules, each enforced before anything is sent:
//   - The bot token is read from -token-file only: a regular file readable
//     by its owner alone, outside any git work tree. It is never printed,
//     never passed in argv or the environment, and HTTP errors are reported
//     without request headers. Requests go only to Discord's API, and
//     redirects are refused.
//   - Preflight: GET /users/@me must be a bot, and GET /channels/<id> must be
//     a text channel of -guild whose @everyone overwrite denies View
//     Channel and whose other overwrites allow View Channel only to the bot
//     itself and to the role and member IDs passed with -allow.
//   - The bot, guild, channel and message count are shown on /dev/tty and
//     the channel name must be typed back; without a terminal it refuses.
//   - Messages go only to -channel, at least 1.2 s apart, honouring 429
//     Retry-After, with allowed_mentions parsing nothing.
//   - Output goes only to -out (absolute, outside any git work tree):
//     run.json (message IDs, for -cleanup; never commit it) and
//     observations.md (no Discord IDs, names or local paths).
//   - -cleanup deletes only the message IDs this run's run.json recorded, in
//     its channel, after the same preflight and confirmation, and only when
//     the token's bot is the one that sent them.
//
// Exit codes: 0 success, 1 failure, 64 usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 64

	discordAPI = "https://discord.com/api/v10"
	sendGap    = 1200 * time.Millisecond // minimum spacing between requests
	embedWait  = 20 * time.Second        // how long after a send embeds are polled for
	pollGap    = 2 * time.Second
)

// env is everything the command touches outside its arguments, so tests can
// substitute each part.
type env struct {
	stdout, stderr io.Writer
	http           *http.Client
	apiBase        string
	openTTY        func() (io.ReadWriteCloser, error)
	now            func() time.Time
	sleep          func(context.Context, time.Duration) error
}

func defaultEnv() *env {
	return &env{
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		http:    &http.Client{Timeout: 30 * time.Second},
		apiBase: discordAPI,
		openTTY: func() (io.ReadWriteCloser, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) },
		now:     time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, defaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}

// options are the parsed flags.
type options struct {
	payloads, tokenFile, guild, channel, phase, out string
	cleanup                                         bool
	allow                                           []string // role and member IDs that may hold View Channel
}

var snowflake = regexp.MustCompile(`^[0-9]{15,21}$`)

var phases = map[string]bool{"grammar": true, "reply": true, "bidi": true, "all": true}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("discordharness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.payloads, "payloads", "", "payloads.json written by TestHarnessPayloads (absolute path)")
	fs.StringVar(&o.tokenFile, "token-file", "", "file holding the bot token, mode 0600, outside any git work tree (absolute path)")
	fs.StringVar(&o.guild, "guild", "", "ID of the private test guild")
	fs.StringVar(&o.channel, "channel", "", "ID of the private text channel; the only channel messages go to")
	fs.StringVar(&o.phase, "phase", "", "grammar, reply, bidi or all")
	fs.StringVar(&o.out, "out", "", "output directory for run.json and observations.md (absolute, outside any git work tree)")
	fs.BoolVar(&o.cleanup, "cleanup", false, "delete the messages recorded in -out's run.json instead of sending")
	allow := fs.String("allow", "", "comma-separated role or member IDs whose View Channel allow on -channel you accept (only you, or a role only you hold)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	var errs []error
	if *allow != "" {
		for _, id := range strings.Split(*allow, ",") {
			if !snowflake.MatchString(id) {
				errs = append(errs, errors.New("-allow: want comma-separated numeric IDs"))
				break
			}
			o.allow = append(o.allow, id)
		}
	}
	if fs.NArg() > 0 {
		errs = append(errs, fmt.Errorf("unexpected arguments %q", fs.Args()))
	}
	abs := func(name, v string) {
		if !filepath.IsAbs(v) {
			errs = append(errs, fmt.Errorf("-%s: want an absolute path", name))
		}
	}
	abs("token-file", o.tokenFile)
	abs("out", o.out)
	if !snowflake.MatchString(o.guild) {
		errs = append(errs, errors.New("-guild: want a numeric ID"))
	}
	if !snowflake.MatchString(o.channel) {
		errs = append(errs, errors.New("-channel: want a numeric ID"))
	}
	if o.cleanup {
		if o.payloads != "" || o.phase != "" {
			errs = append(errs, errors.New("-cleanup takes no -payloads or -phase"))
		}
	} else {
		abs("payloads", o.payloads)
		if !phases[o.phase] {
			errs = append(errs, errors.New("-phase: want grammar, reply, bidi or all"))
		}
	}
	return o, errors.Join(errs...)
}

func run(ctx context.Context, e *env, args []string) int {
	o, err := parseFlags(args, e.stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(e.stderr, "discordharness: %v\n", err)
		}
		return exitUsage
	}
	var token string
	if o.cleanup {
		err = cleanup(ctx, e, o, &token)
	} else {
		err = send(ctx, e, o, &token)
	}
	if err != nil {
		msg := err.Error()
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "[token]") // defence in depth; no error carries it
		}
		fmt.Fprintf(e.stderr, "discordharness: %s\n", msg)
		return exitFail
	}
	return exitOK
}

// prepare checks the output directory and the token, then runs the
// preflight. token is set as soon as it is read, so run can scrub it.
func prepare(ctx context.Context, e *env, o options, token *string) (*client, target, error) {
	if err := checkOutDir(o.out); err != nil {
		return nil, target{}, err
	}
	t, err := readToken(o.tokenFile)
	if err != nil {
		return nil, target{}, err
	}
	*token = t
	c := newClient(e, t)
	tg, err := preflight(ctx, c, o.guild, o.channel, o.allow)
	if err != nil {
		return nil, target{}, err
	}
	fmt.Fprintf(e.stdout, "preflight ok: bot %q, guild %q, channel #%s (private: @everyone is denied View Channel, and only the bot and -allow are allowed it)\n", tg.botName, tg.guildName, printable(tg.channelName))
	return c, tg, nil
}

func send(ctx context.Context, e *env, o options, token *string) error {
	pf, err := loadPayloads(o.payloads)
	if err != nil {
		return err
	}
	cases := pf.phase(o.phase)
	if len(cases) == 0 {
		return fmt.Errorf("%s: no cases for phase %s", filepath.Base(o.payloads), o.phase)
	}
	if _, err := os.Lstat(filepath.Join(o.out, runFile)); err == nil {
		return fmt.Errorf("-out already holds a %s; clean that run up or use a new directory", runFile)
	}
	c, tg, err := prepare(ctx, e, o, token)
	if err != nil {
		return err
	}
	if err := confirm(e, tg, fmt.Sprintf("send %d messages", len(cases))); err != nil {
		return err
	}
	rec := &runRecord{Version: 1, Started: e.now().UTC(), Phase: o.phase, BotID: tg.botID, GuildID: tg.guildID, ChannelID: tg.channelID}
	err = sendAll(ctx, c, e, o.out, tg.channelID, cases, rec)
	if werr := writeRun(o.out, rec); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return fmt.Errorf("%w (%d messages sent and recorded in %s; -cleanup deletes them)", err, len(rec.Messages), runFile)
	}
	if err := writeObservations(o.out, rec, cases); err != nil {
		return err
	}
	failed := 0
	for _, m := range rec.Messages {
		for _, f := range m.FailedChecks {
			failed++
			fmt.Fprintf(e.stdout, "API check failed: %s: %s\n", m.Case, f)
		}
	}
	fmt.Fprintf(e.stdout, "sent %d messages, %d API check failures; wrote %s and %s\n", len(rec.Messages), failed, runFile, observationsFile)
	return nil
}

func cleanup(ctx context.Context, e *env, o options, token *string) error {
	rec, err := readRun(o.out)
	if err != nil {
		return err
	}
	c, tg, err := prepare(ctx, e, o, token)
	if err != nil {
		return err
	}
	switch {
	case rec.ChannelID != tg.channelID || rec.GuildID != tg.guildID:
		return fmt.Errorf("%s was recorded for another guild or channel; refusing", runFile)
	case rec.BotID != tg.botID:
		return fmt.Errorf("%s was recorded by another bot; refusing", runFile)
	}
	var todo []int
	for i, m := range rec.Messages {
		if !m.Deleted {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		fmt.Fprintf(e.stdout, "nothing to delete: every message in %s is already deleted\n", runFile)
		return nil
	}
	if err := confirm(e, tg, fmt.Sprintf("delete %d messages this harness run sent", len(todo))); err != nil {
		return err
	}
	n := 0
	for _, i := range todo {
		m := &rec.Messages[i]
		gone, err := c.deleteMessage(ctx, tg.channelID, m.MessageID)
		if err != nil {
			if werr := writeRun(o.out, rec); werr != nil {
				err = errors.Join(err, werr)
			}
			return fmt.Errorf("%s: %w (%d deleted)", m.Case, err, n)
		}
		m.Deleted = true
		if !gone {
			n++
		}
	}
	if err := writeRun(o.out, rec); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "deleted %d messages (%d were already gone)\n", n, len(todo)-n)
	return nil
}
