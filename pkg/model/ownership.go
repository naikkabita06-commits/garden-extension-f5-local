package model

const ManagedByValue = "gardener-extension-f5"

// OwnershipFor derives provider ownership metadata for a Kubernetes owner and
// target resource role.
func OwnershipFor(owner Owner, clusterUID, resourceRole, sharedGroup string) Ownership {
	return Ownership{
		ManagedBy:       ManagedByValue,
		ClusterUID:      clusterUID,
		SourceKind:      owner.Kind,
		SourceNamespace: owner.Namespace,
		SourceName:      owner.Name,
		SourceUID:       owner.UID,
		ResourceRole:    resourceRole,
		SharedGroup:     sharedGroup,
	}
}
