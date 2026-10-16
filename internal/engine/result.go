package engine

// The enums below have the values of their linkclean counterparts; the root
// package converts by value.

// Skip is linkclean.SkipReason.
type Skip uint8

const (
	NotSkipped Skip = iota
	SkipScheme
	SkipUnparseable
	SkipAmbiguous
	SkipTooLong
	SkipArchive
	SkipRoundCap
)

// Category is linkclean.Category.
type Category uint8

const (
	CatTracker Category = iota + 1
	CatAffiliate
	CatPathSegment
	CatRedirectUnwrapped
)

// Part is linkclean.Part.
type Part uint8

const (
	PartPath Part = iota + 1
	PartQuery
	PartFragment
	PartNested
)

// KeepReason is linkclean.KeepReason.
type KeepReason uint8

const (
	KeptByRule KeepReason = iota + 1
	KeptAffiliate
	KeptSignedGuard
)

// Beneficiary is linkclean.Beneficiary.
type Beneficiary struct{ Network, ID string }

// Hop is linkclean.Hop.
type Hop struct {
	From, To, Rule string
	Category       Category
	FromHost       string
	Beneficiary    Beneficiary
}

// Removal is linkclean.Removal.
type Removal struct {
	Part              Part
	Category          Category
	Name, Value, Text string
	NestedIn, Rule    string
	Beneficiary       Beneficiary
}

// Kept is linkclean.Kept.
type Kept struct {
	Name   string
	Reason KeepReason
	Rule   string
}

// Result is linkclean.Result without the fields the root derives (Input,
// Changed).
type Result struct {
	URL             string
	Skipped         Skip
	Hops            []Hop
	UnwrapAbandoned bool
	Signed          bool
	Removed         []Removal
	Kept            []Kept
}
