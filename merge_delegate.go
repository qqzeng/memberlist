package memberlist

// MergeDelegate is used to involve a client in
// a potential cluster merge operation. Namely, when
// a node does a TCP push/pull (as part of a join),
// the delegate is involved and allowed to cancel the join
// based on custom logic. The merge delegate is NOT invoked
// as part of the push-pull anti-entropy.
type MergeDelegate interface {
	// NotifyMerge is invoked when a merge could take place.
	// Provides a list of the nodes known by the peer. If
	// the return value is non-nil, the merge is canceled.
	// NotifyMerge 用于在执行状态数据的 merge 操作时，上层应用自定义的逻辑，比如可以取消本次的 merge 操作。
	NotifyMerge(peers []*Node) error
}
