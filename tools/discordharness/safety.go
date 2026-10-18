package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// gitTree reports the nearest ancestor of path (itself included) that holds
// a .git entry, checking both path as given and with symlinks resolved.
func gitTree(path string) (string, bool) {
	paths := []string{filepath.Clean(path)}
	if r, err := filepath.EvalSymlinks(path); err == nil && r != paths[0] {
		paths = append(paths, r)
	}
	for _, p := range paths {
		for d := p; ; d = filepath.Dir(d) {
			if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
				return d, true
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", false
}

// readToken reads the bot token from path. The file must be a regular file
// with no group or other permission bits, outside any git work tree. No
// error includes the file's content.
func readToken(path string) (string, error) {
	if _, in := gitTree(path); in {
		return "", errors.New("-token-file is inside a git work tree; keep the token outside every repository")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("-token-file: %v", err)
	}
	switch {
	case !fi.Mode().IsRegular():
		return "", errors.New("-token-file is not a regular file")
	case fi.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("-token-file has mode %04o; it must be readable by its owner only (chmod 600)", fi.Mode().Perm())
	case fi.Size() > 1024:
		return "", errors.New("-token-file is too large to hold one bot token")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("-token-file: %v", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("-token-file is empty")
	}
	for i := 0; i < len(t); i++ {
		if t[i] <= ' ' || t[i] > '~' {
			return "", errors.New("-token-file holds more than one printable ASCII token")
		}
	}
	return t, nil
}

// checkOutDir creates the output directory, which must be outside any git
// work tree so harness output is never committed by accident.
func checkOutDir(dir string) error {
	if _, in := gitTree(dir); in {
		return errors.New("-out is inside a git work tree; use a directory outside every repository")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("-out: %v", err)
	}
	if _, in := gitTree(dir); in { // a symlink created meanwhile
		return errors.New("-out is inside a git work tree; use a directory outside every repository")
	}
	return nil
}

// target is the preflight's verified destination.
type target struct {
	botID, botName         string
	guildID, guildName     string
	channelID, channelName string
}

// preflight checks the token's user is a bot and the channel is a private
// text channel of the guild: its @everyone overwrite (role ID == guild ID)
// denies View Channel.
func preflight(ctx context.Context, c *client, guildID, channelID string) (target, error) {
	var me user
	if err := c.do(ctx, "GET", "/users/@me", nil, &me); err != nil {
		return target{}, fmt.Errorf("preflight: %w", err)
	}
	if !me.Bot || !snowflake.MatchString(me.ID) {
		return target{}, errors.New("preflight: the token is not a bot's")
	}
	var ch channel
	if err := c.do(ctx, "GET", "/channels/"+channelID, nil, &ch); err != nil {
		return target{}, fmt.Errorf("preflight: %w", err)
	}
	switch {
	case ch.ID != channelID:
		return target{}, errors.New("preflight: the channel response names another channel")
	case ch.GuildID != guildID:
		return target{}, errors.New("preflight: the channel is not in -guild")
	case ch.Type != guildText:
		return target{}, fmt.Errorf("preflight: the channel has type %d, not a text channel (0)", ch.Type)
	}
	private := false
	for _, o := range ch.Overwrites {
		if o.ID == guildID && o.Type == 0 {
			deny, err := strconv.ParseUint(o.Deny, 10, 64)
			private = err == nil && deny&viewChannel != 0
		}
	}
	if !private {
		return target{}, errors.New("preflight: the channel's @everyone overwrite does not deny View Channel; use a private channel")
	}
	var g guild
	if err := c.do(ctx, "GET", "/guilds/"+guildID, nil, &g); err != nil {
		return target{}, fmt.Errorf("preflight: %w", err)
	}
	if g.ID != guildID {
		return target{}, errors.New("preflight: the guild response names another guild")
	}
	return target{
		botID: me.ID, botName: printable(me.Username),
		guildID: guildID, guildName: printable(g.Name),
		channelID: channelID, channelName: printable(ch.Name),
	}, nil
}

// confirm shows what is about to happen on the terminal and requires the
// channel name typed back. Without a terminal it refuses.
func confirm(e *env, t target, action string) error {
	tty, err := e.openTTY()
	if err != nil {
		return fmt.Errorf("cannot open /dev/tty to confirm (%v); run this from an interactive terminal", err)
	}
	defer tty.Close()
	fmt.Fprintf(tty, "About to %s as bot %q in guild %q, channel #%s.\nType the channel name to continue: ", action, t.botName, t.guildName, t.channelName)
	line, err := bufio.NewReader(io.LimitReader(tty, 1024)).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no confirmation typed; nothing done")
	}
	if strings.TrimRight(line, "\r\n") != t.channelName || t.channelName == "" {
		return errors.New("the typed name does not match the channel name; nothing done")
	}
	return nil
}
