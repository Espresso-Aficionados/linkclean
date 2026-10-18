package data

// Messages that the gates, rulegen and the review routine print or match
// on. They are fixed text: the routine and the CI logs search for them, so
// change one only together with every reader. A "%" verb list is given per
// format.

// Provenance and sync. The root module's provenance gate (rules_*_test.go)
// keeps an identical copy of the Shared formats; a test checks they match.
const (
	MsgHeld             = "held: %s %s (%s, expires %s)"                                 // file, sha256 (12 hex), reason, expires
	MsgBackwards        = "gitlink moved backwards: %s is not an ancestor of %s"         // B, G
	MsgUnsynced         = "vendored bytes ≠ upstream@%s; run `rulegen sync`"             // G
	MsgUnreachable      = "upstream unreachable; re-run the job"                         //
	MsgInputsUnchanged  = "provenance inputs unchanged from base"                        //
	MsgNoChange         = "no change: upstream@%s has the locked bytes (last synced %s)" // G, X
	MsgUnverifiable     = "unverifiable: %s"                                             // StatusGitlinkMissing or "unreachable"
	MsgUnverifiableDown = "unverifiable: unreachable"                                    //
	MsgUnverifiableGone = "unverifiable: gitlink-missing"                                //

	MsgGitlinkMissing    = "upstream does not serve the gitlink %s (gitlink-missing)"       // G
	MsgNotOnMaster       = "gitlink %s is not an ancestor of upstream master"               // G
	MsgHistoryRewritten  = "upstream history rewritten: %s is no longer on master"          // B
	MsgGitmodulesMapping = ".gitmodules does not map %s to %s with branch = master: %s"     // path, URL, problem
	MsgGitmodulesChanged = ".gitmodules differs from base's"                                // root gate only
	MsgLockNotAncestor   = "lock commit %s is neither the gitlink %s nor an ancestor of it" // X, G; root gate only
	MsgNotGitlink        = "%s is not a gitlink (mode 160000)"                              // submodule path; root gate only
)

// Shared lists the formats the root provenance gate must spell identically.
var Shared = []string{
	MsgHeld, MsgBackwards, MsgUnsynced, MsgUnreachable, MsgInputsUnchanged,
	MsgGitlinkMissing, MsgNotOnMaster, MsgHistoryRewritten, MsgGitmodulesMapping,
	MsgGitmodulesChanged, MsgLockNotAncestor, MsgNotGitlink,
}

// The routine's sync commit.
const (
	SyncSubject       = "rules: brave/adblock-lists@%s (%s)" // G (12 hex), G's commit date (YYYY-MM-DD)
	MergeTrailerValue = "main"                               // Rulegen-Merge: main
	ShortLen          = 12                                   // hex digits of a short commit id or sha256
)

// release.
const (
	MsgUnreleasedNonData = "unreleased non-data changes since %s: %s; release manually (§11.7 manual)" // L, paths
	MsgNoDataChange      = "no data change since %s; nothing to release"                               // L
	MsgPushCI            = "push CI on %s is %s"                                                       // S, state
	MsgManualRelease     = "manual release needed"                                                     //
)

// summary.
const (
	SumNoDataChange     = "No data change: upstream@%s has the locked bytes"        // G
	SumNotSynced        = "Not synced: run `rulegen sync`"                          //
	SumHistoryRewritten = "Upstream history rewritten: %s is no longer on `master`" // B
	SumHeld             = "Held"                                                    //
	SumHoldLapsed       = "Hold lapsed"                                             //
	SumHoldBytesChanged = "hold lapsed: vendored bytes changed (%s → %s)"           // X, Y
	SumHoldExpired      = "hold expired; held bytes now proposed"                   //
	SumHoldStale        = "stale hold"                                              //
	SumAckLapsed        = "ack lapsed (bound to %s)"                                // old sha256
	SumAckExpired       = "ack expired"                                             //
	SumDisableRules     = "DisableRules impact: these IDs now produce Warnings"     //
	SumWatchChanged     = "brave-core conditional/exempt block changed"             //
	SumParseError       = "parse error at byte %d"                                  // offset
	SumExactPath        = "matches one exact path only"                             //
	SumMixesClasses     = "rule now mixes classes"                                  //
	SumCompareURL       = "https://github.com/brave/adblock-lists/compare/%s...%s"  // old, new
	SumWatchSource      = "components/query_filter/browser/utils.cc"                // brave-core path
	SumWatchConditional = "kConditionalQueryStringTrackers"                         //
	SumWatchExempt      = "kExemptedHostnames"                                      //
)

// Summary pair flags.
const (
	FlagRisky     = "⚠"
	FlagCorpus    = "★"
	FlagAffiliate = "⚑"
)

// The review routine's comments and alerts (ROUTINE.md quotes them).
const (
	AlertNoPR             = "no Dependabot submodule PR in 14 days although upstream moved past %s (%s)"                                       // G, "G..M"
	AlertWaitingEdited    = "data path waiting on #%d (human-edited, open %d days; Dependabot opens no newer PR until it is merged or closed)" // n, days
	AlertWaitingEscalated = "data path waiting on #%d (escalated, open %d days; Dependabot opens no newer PR until it is merged or closed)"    // n, days
	AlertAutoMergeArmed   = "auto-merge armed by maintainer"                                                                                   //
	AlertOffMaster        = "its gitlink is off the rewritten `master`; close it"                                                              //
	AlertUnverifiedHead   = "#%d merged at head %s, not the verified head %s; review before any release"                                       // n, sha, V
	AlertMergeRefused     = "merge of #%d at %s refused: %s; not retried"                                                                      // n, sha, gh's error
	AlertReleased         = "linkclean rules %s released: +%d/−%d params, upstream %s; sprobot PR #%d"                                         // tag, A, R, short, m
	CommentGone           = "gitlink left upstream master; closing so Dependabot can propose the rewritten master (§11.6)"                     //
	CommentRulesMoved     = "main's rules moved (takeover/other sync) since this PR was opened; closing so that Dependabot re-proposes upstream against current main (§11.6)"
	CommentRulesMovedPast = "main's rules moved (takeover/other sync) past this PR's gitlink; closing so that Dependabot re-proposes upstream against current main (§11.6)"
	CommentSuperseded     = "superseded by #%d"                                                                                             // n
	CommentNoOp           = "no data change (upstream@%s has the locked bytes)"                                                             // G
	CommentHeld           = "upstream change held: %s, expires %s; closing so that Dependabot can propose the next upstream commit (§11.6)" // reason, date
)
