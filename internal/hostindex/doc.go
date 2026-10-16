// Package hostindex maps a host view to the rules whose Include patterns
// could match it: the exact-host node, each "*." suffix of the host and its
// parents, and the "label.*" map (design §4.3 step 1). Rules with a global
// ("*") Include are not indexed; the caller treats them as candidates for
// every host. Ordinals come out ascending and deduplicated; no map iteration
// feeds output.
package hostindex
