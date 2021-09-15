package memberlist

// ConflictDelegate is a used to inform a client that
// a node has attempted to join which would result in a
// name conflict. This happens if two clients are configured
// with the same name but different addresses.
// ConflictDelegate 用于通知上层应用当一个节点加入集群时，会产生节点名称的冲突。
// 这在两个节点具有相同的节点名称，但具有不再的 ip 地址时发生。
type ConflictDelegate interface {
	// NotifyConflict is invoked when a name conflict is detected
	// 当新加入的节点的名称同已有集群中节点的名称冲突时，会回调该 hook。
	NotifyConflict(existing, other *Node)
}
