package v1alpha1

import (
	"fmt"
	"github.com/mitchellh/hashstructure/v2"
)

// NodeClassHashVersion identifies the revision of the hashing scheme below.
//
// Bump it whenever a change to mustHash or to VultrNodeClassSpec would alter
// the hash of an unchanged NodeClass. Drift is only evaluated between a
// NodeClaim and a NodeClass recorded at the same version, so bumping this
// prevents a provider upgrade from drifting — and therefore replacing — every
// node in the cluster at once.
const NodeClassHashVersion = "v1"

func (in *VultrNodeClass) Hash() string { return fmt.Sprint(mustHash(in.Spec)) }
func mustHash(v any) uint64 {
	h, err := hashstructure.Hash(v, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true, IgnoreZeroValue: true, ZeroNil: true})
	if err != nil {
		panic(err)
	}
	return h
}
