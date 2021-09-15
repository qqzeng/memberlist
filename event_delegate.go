package memberlist

// EventDelegate is a simpler delegate that is used only to receive
// notifications about members joining and leaving. The methods in this
// delegate may be called by multiple goroutines, but never concurrently.
// This allows you to reason about ordering.
type EventDelegate interface {
	// NotifyJoin is invoked when a node is detected to have joined.
	// The Node argument must not be modified.
	// 当节点发现有目标节点加入集群时则会触发回调该 hook。
	// 即目标节点状态变化： dead/left -> alive
	NotifyJoin(*Node)

	// NotifyLeave is invoked when a node is detected to have left.
	// The Node argument must not be modified.
	// 当节点发现有目标节点离开集群时则会触发回调该 hook。
	// 即目标节点状态变化： alive -> dead/left
	NotifyLeave(*Node)

	// NotifyUpdate is invoked when a node is detected to have
	// updated, usually involving the meta data. The Node argument
	// must not be modified.
	// 当节点发现有目标节点的元素信息（ip地址、端口等）发生变更时则会触发回调该 hook。
	NotifyUpdate(*Node)
}

// ChannelEventDelegate is used to enable an application to receive
// events about joins and leaves over a channel instead of a direct
// function call.
//
// Care must be taken that events are processed in a timely manner from
// the channel, since this delegate will block until an event can be sent.
type ChannelEventDelegate struct {
	Ch chan<- NodeEvent
}

// NodeEventType are the types of events that can be sent from the
// ChannelEventDelegate.
type NodeEventType int

const (
	NodeJoin NodeEventType = iota
	NodeLeave
	NodeUpdate
)

// NodeEvent is a single event related to node activity in the memberlist.
// The Node member of this struct must not be directly modified. It is passed
// as a pointer to avoid unnecessary copies. If you wish to modify the node,
// make a copy first.
type NodeEvent struct {
	Event NodeEventType
	Node  *Node
}

func (c *ChannelEventDelegate) NotifyJoin(n *Node) {
	node := *n
	c.Ch <- NodeEvent{NodeJoin, &node}
}

func (c *ChannelEventDelegate) NotifyLeave(n *Node) {
	node := *n
	c.Ch <- NodeEvent{NodeLeave, &node}
}

func (c *ChannelEventDelegate) NotifyUpdate(n *Node) {
	node := *n
	c.Ch <- NodeEvent{NodeUpdate, &node}
}
