package memberlist

// AliveDelegate is used to involve a client in processing
// a node "alive" message. When a node joins, either through
// a UDP gossip or TCP push/pull, we update the state of
// that node via an alive message. This can be used to filter
// a node out and prevent it from being considered a peer
// using application specific logic.
// 当节点从集群中收到 alive 消息时，上层应用可针对性制定一些逻辑来处理。
type AliveDelegate interface {
	// NotifyAlive is invoked when a message about a live
	// node is received from the network.  Returning a non-nil
	// error prevents the node from being considered a peer.
	// 当节点收到 alive 消息时，会回调该接口，若该接口返回错误，则应该忽略该消息，不将目标节点视为集群成员。
	NotifyAlive(peer *Node) error
}
