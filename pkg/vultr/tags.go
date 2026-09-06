package vultr

import "strings"

// Tag prefixes applied to every instance this provider creates. Vultr tags are
// a flat list of strings, so ownership metadata is encoded as `key=value`.
const (
	// TagClusterPrefix scopes an instance to a single Karpenter installation.
	// Without it, two Karpenter clusters sharing one Vultr account would each
	// treat the other's instances as their own and garbage-collect them.
	TagClusterPrefix = "karpenter-cluster="
	// TagNodeClaimPrefix records the Karpenter NodeClaim the instance backs.
	TagNodeClaimPrefix = "karpenter-nodeclaim="
	TagNodePoolPrefix  = "karpenter-nodepool="
	TagNodeClassPrefix = "karpenter-nodeclass="
)

// InstanceTags builds the tag set applied to every instance this provider creates.
func InstanceTags(clusterName, nodeClaim, nodePool, nodeClass string) []string {
	return []string{
		TagClusterPrefix + clusterName,
		TagNodeClaimPrefix + nodeClaim,
		TagNodePoolPrefix + nodePool,
		TagNodeClassPrefix + nodeClass,
	}
}

// TagValue returns the value of the first tag carrying the given prefix.
func TagValue(tags []string, prefix string) (string, bool) {
	for _, tag := range tags {
		if strings.HasPrefix(tag, prefix) {
			value := strings.TrimPrefix(tag, prefix)
			if strings.ContainsAny(value, " \t\r\n") {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

// NodeClaimName returns the NodeClaim an instance was created for, and whether
// the instance belongs to the named Karpenter installation at all. Instances
// without a matching cluster tag are treated as someone else's and are never
// listed, adopted or deleted by this provider.
func NodeClaimName(i *Instance, clusterName string) (string, bool) {
	cluster, ok := TagValue(i.Tags, TagClusterPrefix)
	if !ok || cluster == "" || cluster != clusterName {
		return "", false
	}
	name, ok := TagValue(i.Tags, TagNodeClaimPrefix)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}
