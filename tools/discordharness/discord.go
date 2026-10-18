package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// client is a minimal Discord REST client: one request at a time, at least
// sendGap apart, retrying only on 429.
type client struct {
	e         *env
	hc        *http.Client
	token     string // never printed, logged or put in an error
	last      time.Time
	notBefore time.Time
}

const maxRetryAfter = 120 * time.Second

func newClient(e *env, token string) *client {
	hc := *e.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("redirect refused") // never carry the token elsewhere
	}
	return &client{e: e, hc: &hc, token: token}
}

// apiError is a non-2xx response: status and Discord's error code and
// message, never request headers.
type apiError struct {
	method, path string
	status, code int
	message      string
}

func (a *apiError) Error() string {
	s := fmt.Sprintf("%s %s: HTTP %d", a.method, a.path, a.status)
	if a.code != 0 || a.message != "" {
		s += fmt.Sprintf(" (Discord code %d: %s)", a.code, a.message)
	}
	return s
}

// do sends one request and decodes a 2xx JSON response into out.
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		if err := c.wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, c.e.apiBase+path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("%s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bot "+c.token)
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/Espresso-Aficionados/linkclean, harness)")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		c.last = c.e.now()
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // without the URL, which repeats method and path
			}
			return fmt.Errorf("%s %s: %v", method, path, err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		c.rateHeaders(resp.Header)
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			d, ok := retryAfter(resp.Header, data)
			if !ok || d > maxRetryAfter {
				return &apiError{method: method, path: path, status: resp.StatusCode, message: "rate limited without a usable Retry-After"}
			}
			if t := c.e.now().Add(d); t.After(c.notBefore) {
				c.notBefore = t
			}
			continue
		}
		if rerr != nil {
			return fmt.Errorf("%s %s: reading the response: %v", method, path, rerr)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			a := &apiError{method: method, path: path, status: resp.StatusCode}
			var de struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(data, &de) == nil {
				a.code, a.message = de.Code, printable(de.Message)
			}
			return a
		}
		if out != nil && resp.StatusCode != http.StatusNoContent {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("%s %s: decoding the response: %v", method, path, err)
			}
		}
		return nil
	}
}

// wait sleeps until the request spacing and any rate-limit reset allow the
// next request.
func (c *client) wait(ctx context.Context) error {
	at := c.notBefore
	if !c.last.IsZero() && c.last.Add(sendGap).After(at) {
		at = c.last.Add(sendGap)
	}
	if d := at.Sub(c.e.now()); d > 0 {
		return c.e.sleep(ctx, d)
	}
	return nil
}

// rateHeaders defers the next request when the bucket is empty.
func (c *client) rateHeaders(h http.Header) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	if s, err := strconv.ParseFloat(h.Get("X-RateLimit-Reset-After"), 64); err == nil && s > 0 && s <= maxRetryAfter.Seconds() {
		if t := c.e.now().Add(time.Duration(s * float64(time.Second))); t.After(c.notBefore) {
			c.notBefore = t
		}
	}
}

// retryAfter reads a 429's wait: the Retry-After header (seconds), else
// the body's retry_after.
func retryAfter(h http.Header, body []byte) (time.Duration, bool) {
	s, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Retry-After")), 64)
	if err != nil {
		var b struct {
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(body, &b) != nil || b.RetryAfter <= 0 {
			return 0, false
		}
		s = b.RetryAfter
	}
	if s < 0 {
		return 0, false
	}
	return max(time.Duration(s*float64(time.Second)), sendGap), true
}

// printable drops control and format characters from text Discord returns
// before it reaches the terminal, and caps its length.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// The Discord objects the harness reads.
type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type overwrite struct {
	ID   string `json:"id"`
	Type int    `json:"type"` // 0 role, 1 member
	Deny string `json:"deny"`
}

type channel struct {
	ID         string      `json:"id"`
	Type       int         `json:"type"`
	GuildID    string      `json:"guild_id"`
	Name       string      `json:"name"`
	Overwrites []overwrite `json:"permission_overwrites"`
}

type guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type embed struct {
	Type  string `json:"type,omitempty"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

type message struct {
	ID        string  `json:"id"`
	ChannelID string  `json:"channel_id"`
	Content   string  `json:"content"`
	Flags     int     `json:"flags"`
	Embeds    []embed `json:"embeds"`
}

// outgoing is the body of POST /channels/{id}/messages.
type outgoing struct {
	Content          string            `json:"content"`
	Flags            int               `json:"flags,omitempty"`
	AllowedMentions  allowedMentions   `json:"allowed_mentions"`
	MessageReference *messageReference `json:"message_reference,omitempty"`
}

type allowedMentions struct {
	Parse       []string `json:"parse"`
	RepliedUser bool     `json:"replied_user"`
}

type messageReference struct {
	MessageID       string `json:"message_id"`
	FailIfNotExists bool   `json:"fail_if_not_exists"`
}

const (
	viewChannel    = 1 << 10
	guildText      = 0
	flagSuppress   = 1 << 2
	unknownMessage = 10008
)

func (c *client) postMessage(ctx context.Context, channelID string, o outgoing) (message, error) {
	var m message
	err := c.do(ctx, http.MethodPost, "/channels/"+channelID+"/messages", o, &m)
	if err == nil && (m.ID == "" || m.ChannelID != channelID) {
		err = errors.New("POST message: the response names no message in this channel")
	}
	return m, err
}

func (c *client) getMessage(ctx context.Context, channelID, id string) (message, error) {
	var m message
	err := c.do(ctx, http.MethodGet, "/channels/"+channelID+"/messages/"+id, nil, &m)
	return m, err
}

// deleteMessage deletes one message; gone reports it was already deleted.
func (c *client) deleteMessage(ctx context.Context, channelID, id string) (gone bool, err error) {
	err = c.do(ctx, http.MethodDelete, "/channels/"+channelID+"/messages/"+id, nil, nil)
	var a *apiError
	if errors.As(err, &a) && a.status == http.StatusNotFound && a.code == unknownMessage {
		return true, nil
	}
	return false, err
}
