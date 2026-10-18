package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	runFile          = "run.json"
	observationsFile = "observations.md"
)

// runRecord is run.json: what was sent and what the API returned. It holds
// Discord IDs, so -cleanup can find the messages; it is never committed.
type runRecord struct {
	Version   int           `json:"version"`
	Started   time.Time     `json:"started"`
	Phase     string        `json:"phase"`
	BotID     string        `json:"bot_id"`
	GuildID   string        `json:"guild_id"`
	ChannelID string        `json:"channel_id"`
	Messages  []sentMessage `json:"messages"`
}

type sentMessage struct {
	Case          string    `json:"case"`
	MessageID     string    `json:"message_id"`
	SentAt        time.Time `json:"sent_at"`
	Content       string    `json:"content"`
	Flags         int       `json:"flags"`
	ReplyTo       string    `json:"reply_to,omitempty"`
	ContentEqual  bool      `json:"content_equal"`  // the returned content is the content sent
	ReturnedFlags int       `json:"returned_flags"` // flags on the message Discord returned
	Embeds        []embed   `json:"embeds"`         // after the poll
	Polled        bool      `json:"polled"`         // embeds were read embedWait after the send, or seen earlier
	FailedChecks  []string  `json:"failed_checks,omitempty"`
	Deleted       bool      `json:"deleted,omitempty"`
}

func (m *sentMessage) observe(got message) {
	m.ContentEqual = got.Content == m.Content
	m.ReturnedFlags = got.Flags
	m.Embeds = got.Embeds
	if m.Embeds == nil {
		m.Embeds = []embed{}
	}
}

// check records the API checks: content and flags as sent, and no embeds
// where the case expects none.
func (m *sentMessage) check(pc payloadCase) {
	m.FailedChecks = nil
	if !m.ContentEqual {
		m.FailedChecks = append(m.FailedChecks, "the returned content differs from the content sent")
	}
	if m.ReturnedFlags&pc.Flags != pc.Flags {
		m.FailedChecks = append(m.FailedChecks, fmt.Sprintf("flags %d were sent, the message has %d", pc.Flags, m.ReturnedFlags))
	}
	if pc.ExpectNoEmbeds && len(m.Embeds) > 0 {
		m.FailedChecks = append(m.FailedChecks, fmt.Sprintf("%d embeds, want none", len(m.Embeds)))
	}
}

// sendAll sends the cases in order, recording each message in run.json as
// soon as it exists, then reads every message back for its embeds.
func sendAll(ctx context.Context, c *client, e *env, out, channelID string, cases []payloadCase, rec *runRecord) error {
	sent := map[string]string{}
	for _, pc := range cases {
		o := outgoing{Content: pc.Content, Flags: pc.Flags, AllowedMentions: allowedMentions{Parse: []string{}}}
		if pc.ReplyTo != "" {
			id, ok := sent[pc.ReplyTo]
			if !ok {
				return fmt.Errorf("%s: its reply target %s was not sent", pc.ID, pc.ReplyTo)
			}
			o.MessageReference = &messageReference{MessageID: id, FailIfNotExists: true}
		}
		got, err := c.postMessage(ctx, channelID, o)
		if err != nil {
			// Only an HTTP 4xx answer proves nothing was created. Otherwise
			// (no answer, an unreadable one, or a 5xx) the message may exist
			// without an ID to record, and -cleanup cannot see it.
			var a *apiError
			if !errors.As(err, &a) || a.status >= 500 {
				return fmt.Errorf("%s: %w; the message may have been created without being recorded, so -cleanup cannot delete it: check the channel and delete it by hand", pc.ID, err)
			}
			return fmt.Errorf("%s: %w", pc.ID, err)
		}
		sent[pc.ID] = got.ID
		m := sentMessage{Case: pc.ID, MessageID: got.ID, SentAt: e.now().UTC(), Content: pc.Content, Flags: pc.Flags, ReplyTo: pc.ReplyTo}
		m.observe(got)
		rec.Messages = append(rec.Messages, m)
		if err := writeRun(out, rec); err != nil {
			return err
		}
	}
	for i := range rec.Messages {
		m := &rec.Messages[i]
		deadline := m.SentAt.Add(embedWait)
		for {
			got, err := c.getMessage(ctx, channelID, m.MessageID)
			if err != nil {
				return fmt.Errorf("%s: %w", m.Case, err)
			}
			m.observe(got)
			now := e.now()
			if len(got.Embeds) > 0 || !now.Before(deadline) {
				break
			}
			if err := e.sleep(ctx, min(pollGap, deadline.Sub(now))); err != nil {
				return err
			}
		}
		m.Polled = true
		m.check(cases[i])
	}
	return nil
}

// writeFile replaces dir/name atomically, readable by the owner only.
func writeFile(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func writeRun(dir string, rec *runRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(dir, runFile, append(b, '\n'))
}

func readRun(dir string) (*runRecord, error) {
	b, err := os.ReadFile(filepath.Join(dir, runFile))
	if err != nil {
		return nil, fmt.Errorf("-out: %v", err)
	}
	var rec runRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("%s: %v", runFile, err)
	}
	if rec.Version != 1 || !snowflake.MatchString(rec.BotID) || !snowflake.MatchString(rec.GuildID) || !snowflake.MatchString(rec.ChannelID) {
		return nil, fmt.Errorf("%s: not a harness run record", runFile)
	}
	for _, m := range rec.Messages {
		if !snowflake.MatchString(m.MessageID) {
			return nil, fmt.Errorf("%s: case %s has no valid message ID", runFile, m.Case)
		}
	}
	return &rec, nil
}

// writeObservations writes observations.md: the API results and empty
// columns for the manual observations. It names cases, contents, flags and
// embeds only: no Discord IDs, bot, guild or channel names, or local paths,
// so the filled table can be committed as a result.
func writeObservations(dir string, rec *runRecord, cases []payloadCase) error {
	var b bytes.Buffer
	date := rec.Started.Format("2006-01-02")
	fmt.Fprintf(&b, "# discordlink harness observations, %s\n\n", date)
	fmt.Fprintf(&b, "Phase `%s`, %d messages, sent %s by `tools/discordharness`. ", rec.Phase, len(rec.Messages), rec.Started.Format(time.RFC3339))
	fmt.Fprintf(&b, "Fill in the empty cells, then commit this file as `tools/discordharness/results/%s.md`. It holds no Discord IDs, names or local paths; keep it that way.\n\n", date)
	b.WriteString(`Clients: the desktop client for every row; also one mobile client (and the web client if convenient) for the rows marked "user copy" and for Phases R and B.

In each row note:

1. **Clickable**: exactly which characters are clickable; hovering (or a long press) shows the target. Note the trailing ` + "`)`, `>`, `*`, `_` and `|`" + ` cases.
2. **Embed**: whether an embed appears, against the API column.
3. **Spoiler**: whether spoilered content, including a URL inside ` + "`||…||`" + `, stays hidden until clicked.
4. **Leave prompt**: whether the "leaving Discord" prompt shows the exact target.
5. **User copy**: paste the content (section "User copies") from your own user account in a normal client, with no automation, and note whether it renders as the bot's message does.

The API column is what Discord returned: whether the content came back unchanged, the message flags, and the embeds read up to 20 s after the send.

`)
	failed := 0
	for _, m := range rec.Messages {
		failed += len(m.FailedChecks)
	}
	fmt.Fprintf(&b, "## API checks\n\n%d failures.\n\n", failed)
	for _, m := range rec.Messages {
		for _, f := range m.FailedChecks {
			fmt.Fprintf(&b, "- %s: %s\n", m.Case, f)
		}
	}
	if failed > 0 {
		b.WriteString("\n")
	}

	byID := map[string]sentMessage{}
	for _, m := range rec.Messages {
		byID[m.Case] = m
	}
	sections := []struct{ phase, title, header string }{
		{"grammar", "Phase G: grammar", "| Case | Source | Content | Find predicts | API | Clickable (desktop) | Clickable (mobile) | Embed | Spoiler | Leave prompt | User copy agrees | Notes |"},
		{"reply", "Phase R: reply format", "| Case | Check | Body | Find predicts | API | Desktop | Mobile | Leave prompt | Notes |"},
		{"bidi", "Phase B: bidi", "| Case | Check | Content | Find predicts | API | Desktop | Mobile | Notes |"},
	}
	for _, s := range sections {
		var rows []string
		for _, pc := range cases {
			m, ok := byID[pc.ID]
			if pc.Phase != s.phase || !ok {
				continue
			}
			cells := []string{pc.ID, mdText(pc.Source), mdCode(pc.Content), predictCell(pc.Predicted), apiCell(m)}
			if len(pc.Markers) > 0 {
				cells[1] += "<br>" + mdText("unverified: "+strings.Join(pc.Markers, "; "))
			}
			blanks := strings.Count(s.header, "|") - 1 - len(cells)
			if s.phase == "grammar" && !pc.UserCopy {
				cells = append(cells, "", "n/a", "", "", "", "n/a", "")
				blanks = 0
			}
			for range blanks {
				cells = append(cells, "")
			}
			rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n|%s\n%s\n\n", s.title, s.header, strings.Repeat("---|", strings.Count(s.header, "|")-1), strings.Join(rows, "\n"))
	}

	var copies []payloadCase
	for _, pc := range cases {
		if _, ok := byID[pc.ID]; ok && pc.UserCopy {
			copies = append(copies, pc)
		}
	}
	if len(copies) > 0 {
		b.WriteString("## User copies\n\nPaste each block's content exactly, as one message, from your own account into the same channel.\n\n")
		for _, pc := range copies {
			fence := strings.Repeat("`", max(3, longestRun(pc.Content, '`')+1))
			fmt.Fprintf(&b, "%s:\n\n%s\n%s\n%s\n\n", pc.ID, fence, pc.Content, fence)
		}
	}
	return writeFile(dir, observationsFile, b.Bytes())
}

// mdText makes s safe in a table cell.
func mdText(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ").Replace(s)
}

// mdCode shows s Go-quoted (so invisible and control characters are
// visible) in a code span that is safe in a table cell.
func mdCode(s string) string {
	q := strings.ReplaceAll(strconv.Quote(s), "|", `\|`)
	fence := strings.Repeat("`", longestRun(q, '`')+1)
	return fence + " " + q + " " + fence
}

func longestRun(s string, c byte) int {
	best, n := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			n++
			best = max(best, n)
		} else {
			n = 0
		}
	}
	return best
}

func predictCell(ls []predictedLink) string {
	if len(ls) == 0 {
		return "no link"
	}
	var parts []string
	for _, l := range ls {
		p := l.Kind + " " + mdCode(l.URL)
		if l.Spoiler {
			p += " spoiler"
		}
		if l.Label != "" {
			p += " label " + mdCode(l.Label)
		}
		if l.WouldEmbed {
			p += " (embeds)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "<br>")
}

func apiCell(m sentMessage) string {
	var parts []string
	if m.ContentEqual {
		parts = append(parts, "content same")
	} else {
		parts = append(parts, "**content changed**")
	}
	parts = append(parts, fmt.Sprintf("flags %d", m.ReturnedFlags))
	if len(m.Embeds) == 0 {
		parts = append(parts, "no embed")
	}
	for _, em := range m.Embeds {
		parts = append(parts, "embed "+mdText(em.Type)+" "+mdCode(em.URL))
	}
	if len(m.FailedChecks) > 0 {
		parts = append(parts, "**check failed**")
	}
	return strings.Join(parts, "<br>")
}
