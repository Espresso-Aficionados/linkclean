package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/data"
	"github.com/Espresso-Aficionados/linkclean/tools/rulegen/internal/repo"
)

// sumUpstream is what the upstream clone says about head.
type sumUpstream struct {
	note      string // why the sync state was not checked; "" when it was
	synced    bool   // every vendored file is the blob at head's gitlink
	unsynced  []string
	rewritten bool // base's gitlink is no longer on upstream master
}

// checkUpstream compares head with the clone: are the vendored bytes the
// blobs at head's gitlink, and is base's gitlink still on master?
func (s *summary) checkUpstream(ctx context.Context) sumUpstream {
	var u sumUpstream
	if s.upstream == "" {
		u.note = "no upstream clone (pass -upstream or set LINKCLEAN_UPSTREAM_DIR); sync state not checked"
		return u
	}
	r := repo.Repo{Git: s.e.Git, Dir: s.upstream}
	status := "missing"
	if b, err := os.ReadFile(filepath.Join(s.upstream, data.StatusFile)); err == nil {
		status, _, _ = strings.Cut(strings.TrimSpace(string(b)), "\n")
	}
	_, haveMaster, err := r.RevParse(ctx, data.MasterRef)
	if err != nil {
		haveMaster = false
	}
	if haveMaster && s.base.gitlink != "" {
		_, present, err := r.RevParse(ctx, s.base.gitlink+"^{commit}")
		on := false
		if err == nil && present {
			on, err = r.IsAncestor(ctx, s.base.gitlink, data.MasterRef)
		}
		// A commit the clone of master lacks is not on master.
		u.rewritten = err == nil && !on
	}
	switch {
	case status != data.StatusOK:
		u.note = fmt.Sprintf("upstream clone STATUS is %q; sync state not checked", status)
		return u
	case s.head.gitlink == "":
		u.note = "head has no gitlink; sync state not checked"
		return u
	}
	if _, ok, err := r.RevParse(ctx, s.head.gitlink+"^{commit}"); err != nil || !ok {
		u.note = fmt.Sprintf("upstream clone lacks the gitlink %s; sync state not checked", sumShort(s.head.gitlink))
		return u
	}
	for _, spec := range data.Files {
		blob, ok, err := r.RevParse(ctx, s.head.gitlink+":"+spec.Upstream)
		if err != nil {
			u.note = fmt.Sprintf("upstream clone: %v; sync state not checked", err)
			return u
		}
		b := s.head.read(spec.Vendored)
		if !ok || b == nil || repo.BlobID(b) != blob {
			u.unsynced = append(u.unsynced, spec.Vendored)
		}
	}
	u.synced = len(u.unsynced) == 0
	return u
}

// changedFiles lists the upstream paths whose head bytes differ from base's
// lock, in lock order.
func (s *summary) changedFiles() []string {
	var out []string
	for _, spec := range data.Files {
		if s.head.sha256Of(spec.Upstream) != s.base.lockSHA256(spec.Upstream) {
			out = append(out, spec.Upstream)
		}
	}
	return out
}

func (s *summary) writeHeader(ctx context.Context, w *sumWriter) {
	w.h("Header")
	lockCommit := func(sd *sumSide) string {
		if sd.lock == nil {
			return ""
		}
		return sd.lock.Upstream.Commit
	}
	bx, hx := lockCommit(s.base), lockCommit(s.head)
	w.f("- Gitlink: %s → %s", sumCode(sumShort(s.base.gitlink)), sumCode(sumShort(s.head.gitlink)))
	w.f("- Last synced: %s → %s", sumCode(sumShort(bx)), sumCode(sumShort(hx)))
	switch {
	case s.base.gitlink != "" && s.head.gitlink != "" && s.base.gitlink != s.head.gitlink:
		w.f("- Compare: "+data.SumCompareURL, s.base.gitlink, s.head.gitlink)
	case bx != "" && hx != "" && bx != hx:
		w.f("- Compare: "+data.SumCompareURL, bx, hx)
	}

	changed := s.changedFiles()
	u := s.checkUpstream(ctx)
	switch {
	case u.note != "":
		w.f("- note: %s", u.note)
	case !u.synced:
		w.f("- **%s** (differs from upstream@%s: %s)", data.SumNotSynced, sumShort(s.head.gitlink), strings.Join(u.unsynced, ", "))
	case len(changed) == 0:
		w.f("- **"+data.SumNoDataChange+"**", sumShort(s.head.gitlink))
	default:
		w.f("- Synced: the vendored bytes are upstream@%s's", sumShort(s.head.gitlink))
	}
	if u.rewritten {
		w.f("- **"+data.SumHistoryRewritten+"**", sumShort(s.base.gitlink))
	}
	for _, n := range append(s.base.notes, s.head.notes...) {
		w.f("- note: %s", n)
	}
	s.writeHolds(w, changed)
	if len(changed) > 0 {
		w.blank()
		w.f("Changed vendored files (sha256, base's lock → head):")
		w.blank()
		for _, up := range changed {
			w.f("- %s: %s → %s", sumCode(up), sumCode(sumShort(s.base.lockSHA256(up))), sumCode(sumShort(s.head.sha256Of(up))))
		}
	}
	var entries []string
	for _, up := range changed {
		if spec, _ := data.SpecOf(up); spec.Kind != data.KindLicense {
			entries = append(entries, fmt.Sprintf(`{"file": %q, "sha256": %q, "expires": "YYYY-MM-DD", "reason": "…", "by": "…"}`, up, s.head.sha256Of(up)))
		}
	}
	if len(entries) > 0 {
		w.blank()
		w.f("Hold entries for %s (ready to paste into `holds`):", data.AcksPath)
		w.blank()
		w.f("```json")
		w.f("%s", strings.Join(entries, ",\n"))
		w.f("```")
	}
}

// writeHolds reports every hold of head's rules/acks.json against the PR:
// held (it matches, so Provenance fails), lapsed in either way release can
// happen, or stale (its file is not changed by the PR).
func (s *summary) writeHolds(w *sumWriter, changed []string) {
	acks, ok := s.headAcks()
	if !ok {
		return
	}
	isChanged := map[string]bool{}
	for _, up := range changed {
		isChanged[up] = true
	}
	for _, h := range acks.Holds {
		spec, ok := data.SpecOf(h.File)
		if !ok || spec.Kind == data.KindLicense {
			continue // rules-schema reports it
		}
		head := s.head.sha256Of(h.File)
		expired := data.Expired(h.Expires, s.now)
		desc := fmt.Sprintf("%s %s (%s, expires %s)", sumCode(h.File), sumCode(sumShort(h.SHA256)), h.Reason, h.Expires)
		switch {
		case !isChanged[h.File]:
			if !expired {
				w.f("- %s: %s; the PR does not change this file", data.SumHoldStale, desc)
			}
		case !expired && head == h.SHA256:
			w.f("- **%s**: "+data.MsgHeld+" `{\"file\": %q, \"sha256\": %q}`", data.SumHeld, h.File, sumShort(h.SHA256), h.Reason, h.Expires, h.File, h.SHA256)
		case !expired:
			w.f("- **%s**: "+data.SumHoldBytesChanged+" for %s", data.SumHoldLapsed, sumShort(h.SHA256), sumShort(head), desc)
		case head == h.SHA256:
			w.f("- **%s**: %s for %s", data.SumHoldLapsed, data.SumHoldExpired, desc)
		}
	}
}

// headAcks decodes head's rules/acks.json, keeping entries that fail
// validation; ok is false when the file is absent or does not decode.
func (s *summary) headAcks() (data.Acks, bool) {
	b := s.head.read(data.AcksPath)
	if b == nil {
		return data.Acks{}, false
	}
	a, errs := data.ParseAcks(b)
	if len(errs) > 0 && a.Schema == 0 && a.Acks == nil && a.Holds == nil {
		return data.Acks{}, false
	}
	return a, true
}
