// Package hostindex maps a host view to the ordinals of the rules whose
// Include patterns could match it: the exact-host node, each "*." suffix
// walked label by label, the "label.*" map and the global bucket (design §4.3
// step 1). Candidates come out ascending and deduplicated; no map iteration
// feeds output.
package hostindex
