package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"unicode/utf8"
)

// payloadFile is payloads.json, as discordlink's TestHarnessPayloads writes
// it (harnessFile there). The halves share no code: a field added there must
// be added here, or loading fails.
type payloadFile struct {
	Version int           `json:"version"`
	Cases   []payloadCase `json:"cases"`
}

type payloadCase struct {
	ID             string          `json:"id"`
	Phase          string          `json:"phase"`
	Source         string          `json:"source"`
	Markers        []string        `json:"markers,omitempty"`
	Content        string          `json:"content"`
	Flags          int             `json:"flags"`
	ReplyTo        string          `json:"reply_to,omitempty"`
	UserCopy       bool            `json:"user_copy,omitempty"`
	ExpectNoEmbeds bool            `json:"expect_no_embeds,omitempty"`
	Predicted      []predictedLink `json:"predicted"`
}

type predictedLink struct {
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	Spoiler    bool   `json:"spoiler,omitempty"`
	Label      string `json:"label,omitempty"`
	WouldEmbed bool   `json:"would_embed"`
}

const (
	maxContent = 2000 // bytes; Discord's limit is 2000 characters
	maxCases   = 200
)

var caseID = regexp.MustCompile(`^[A-Za-z0-9.-]{1,16}$`)

// loadPayloads reads and validates payloads.json. Validation repeats what
// the payload test checks, since this is the half that sends.
func loadPayloads(path string) (*payloadFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-payloads: %v", err)
	}
	var pf payloadFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pf); err != nil {
		return nil, fmt.Errorf("-payloads: %v", err)
	}
	if err := pf.validate(); err != nil {
		return nil, fmt.Errorf("-payloads: %w", err)
	}
	return &pf, nil
}

func (pf *payloadFile) validate() error {
	if pf.Version != 1 {
		return fmt.Errorf("version %d, want 1", pf.Version)
	}
	if len(pf.Cases) == 0 || len(pf.Cases) > maxCases {
		return fmt.Errorf("%d cases, want 1 to %d", len(pf.Cases), maxCases)
	}
	phaseOf := map[string]string{}
	var errs []error
	for _, c := range pf.Cases {
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("case %q: %s", c.ID, fmt.Sprintf(format, args...)))
		}
		if !caseID.MatchString(c.ID) {
			bad("bad ID")
		}
		if _, dup := phaseOf[c.ID]; dup {
			bad("duplicate ID")
		}
		switch c.Phase {
		case "grammar", "reply", "bidi":
		default:
			bad("unknown phase %q", c.Phase)
		}
		switch {
		case c.Content == "" || len(c.Content) > maxContent:
			bad("content is %d bytes, want 1 to %d", len(c.Content), maxContent)
		case !utf8.ValidString(c.Content):
			bad("content is not valid UTF-8")
		}
		if c.Flags != 0 && c.Flags != flagSuppress {
			bad("flags %d; only 0 and 4 (SUPPRESS_EMBEDS) are sent", c.Flags)
		}
		if c.ReplyTo != "" && phaseOf[c.ReplyTo] != c.Phase {
			bad("replies to %q, which is not an earlier case of the same phase", c.ReplyTo)
		}
		phaseOf[c.ID] = c.Phase
	}
	return errors.Join(errs...)
}

// phase returns the cases of one phase, or all of them.
func (pf *payloadFile) phase(p string) []payloadCase {
	var out []payloadCase
	for _, c := range pf.Cases {
		if p == "all" || c.Phase == p {
			out = append(out, c)
		}
	}
	return out
}
