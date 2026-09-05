package v1alpha1

import (
	"fmt"
	"github.com/mitchellh/hashstructure/v2"
)

const NodeClassHashVersion = "v1"

func (in *VultrNodeClass) Hash() string { return fmt.Sprint(mustHash(in.Spec)) }
func mustHash(v any) uint64 {
	h, err := hashstructure.Hash(v, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true, IgnoreZeroValue: true, ZeroNil: true})
	if err != nil {
		panic(err)
	}
	return h
}
