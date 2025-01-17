package memberlist

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strings"
	"sync/atomic"
	"time"

	metrics "github.com/armon/go-metrics"
)

type NodeStateType int

const (
	StateAlive NodeStateType = iota
	StateSuspect
	StateDead
	StateLeft
)

// Node represents a node in the cluster.
type Node struct {
	Name  string
	Addr  net.IP
	Port  uint16
	Meta  []byte        // Metadata from the delegate for this node.
	State NodeStateType // State of the node.
	PMin  uint8         // Minimum protocol version this understands
	PMax  uint8         // Maximum protocol version this understands
	PCur  uint8         // Current version node is speaking
	DMin  uint8         // Min protocol version for the delegate to understand
	DMax  uint8         // Max protocol version for the delegate to understand
	DCur  uint8         // Current version delegate is speaking
}

// Address returns the host:port form of a node's address, suitable for use
// with a transport.
func (n *Node) Address() string {
	return joinHostPort(n.Addr.String(), n.Port)
}

// FullAddress returns the node name and host:port form of a node's address,
// suitable for use with a transport.
func (n *Node) FullAddress() Address {
	return Address{
		Addr: joinHostPort(n.Addr.String(), n.Port),
		Name: n.Name,
	}
}

// String returns the node name
func (n *Node) String() string {
	return n.Name
}

// NodeState is used to manage our state view of another node
// NodeState 用于保存当前节点对集群中其它节点的一个视图数据
type nodeState struct {
	Node
	Incarnation uint32        // Last known incarnation number
	State       NodeStateType // Current state
	StateChange time.Time     // Time last state change happened
}

// Address returns the host:port form of a node's address, suitable for use
// with a transport.
func (n *nodeState) Address() string {
	return n.Node.Address()
}

// FullAddress returns the node name and host:port form of a node's address,
// suitable for use with a transport.
func (n *nodeState) FullAddress() Address {
	return n.Node.FullAddress()
}

func (n *nodeState) DeadOrLeft() bool {
	return n.State == StateDead || n.State == StateLeft
}

// ackHandler is used to register handlers for incoming acks and nacks.
type ackHandler struct {
	ackFn  func([]byte, time.Time)
	nackFn func()
	timer  *time.Timer
}

// NoPingResponseError is used to indicate a 'ping' packet was
// successfully issued but no response was received
type NoPingResponseError struct {
	node string
}

func (f NoPingResponseError) Error() string {
	return fmt.Sprintf("No response from node %s", f.node)
}

// Schedule is used to ensure the Tick is performed periodically. This
// function is safe to call multiple times. If the memberlist is already
// scheduled, then it won't do anything.
// schedule 方法执行一些周期性触发的动作，可调用多次。具体包括三个方面：
// 1. 随机从本地集群成员视图列表中选择一个集群成员，执行故障检测的过程；
// 2. 随机选择一个集群成员，执行全量状态同步交换的过程;
// 3. 随机选择若干个集群成员，执行基于 gossip 传播方式的消息传播过程。
func (m *Memberlist) schedule() {
	m.tickerLock.Lock()
	defer m.tickerLock.Unlock()

	// If we already have tickers, then don't do anything, since we're
	// scheduled
	// 保证只被调用一次
	if len(m.tickers) > 0 {
		return
	}

	// Create the stop tick channel, a blocking channel. We close this
	// when we should stop the tickers.
	// 创建定时任务取消通道
	stopCh := make(chan struct{})

	// Create a new probeTicker
	// 创建定时探测任务，执行故障检测的过程
	if m.config.ProbeInterval > 0 {
		t := time.NewTicker(m.config.ProbeInterval)
		go m.triggerFunc(m.config.ProbeInterval, t.C, stopCh, m.probe)
		m.tickers = append(m.tickers, t)
	}

	// Create a push pull ticker if needed
	// 创建定时全量状态同步交换任务，执行集群中节点间数据同步交换过程
	if m.config.PushPullInterval > 0 {
		go m.pushPullTrigger(stopCh)
	}

	// Create a gossip ticker if needed
	// 创建定时基于 gossip 传播方式的消息传播任务，执行基于 gossip 传播的消息广播过程
	if m.config.GossipInterval > 0 && m.config.GossipNodes > 0 {
		t := time.NewTicker(m.config.GossipInterval)
		go m.triggerFunc(m.config.GossipInterval, t.C, stopCh, m.gossip)
		m.tickers = append(m.tickers, t)
	}

	// If we made any tickers, then record the stopTick channel for
	// later.
	if len(m.tickers) > 0 {
		m.stopTick = stopCh
	}
}

// triggerFunc is used to trigger a function call each time a
// message is received until a stop tick arrives.
// triggerFunc 定时执行传入的操作函数，同时会随机一个开始时间戳
func (m *Memberlist) triggerFunc(stagger time.Duration, C <-chan time.Time, stop <-chan struct{}, f func()) {
	// Use a random stagger to avoid syncronizing
	randStagger := time.Duration(uint64(rand.Int63()) % uint64(stagger))
	select {
	case <-time.After(randStagger):
	case <-stop:
		return
	}
	for {
		select {
		case <-C:
			f()
		case <-stop:
			return
		}
	}
}

// pushPullTrigger is used to periodically trigger a push/pull until
// a stop tick arrives. We don't use triggerFunc since the push/pull
// timer is dynamically scaled based on cluster size to avoid network
// saturation
// pushPullTrigger 用于周期性的触发执行 push->pull->merge 的操作直至定时器被取消。
// 定时器的超时时限是动态变化的。
func (m *Memberlist) pushPullTrigger(stop <-chan struct{}) {
	interval := m.config.PushPullInterval

	// Use a random stagger to avoid syncronizing
	randStagger := time.Duration(uint64(rand.Int63()) % uint64(interval))
	select {
	case <-time.After(randStagger):
	case <-stop:
		return
	}

	// Tick using a dynamic timer
	for {
		// 基于集群规模，动态计算超时时间
		// 一种直观的解释是，当集群成员数目增多时，我们需要扩大同步周期，以避免整个集群网络中充斥着大量的消息。
		tickTime := pushPullScale(interval, m.estNumNodes())
		select {
		case <-time.After(tickTime):
			m.pushPull()
		case <-stop:
			return
		}
	}
}

// Deschedule is used to stop the background maintenance. This is safe
// to call multiple times.
func (m *Memberlist) deschedule() {
	m.tickerLock.Lock()
	defer m.tickerLock.Unlock()

	// If we have no tickers, then we aren't scheduled.
	if len(m.tickers) == 0 {
		return
	}

	// Close the stop channel so all the ticker listeners stop.
	close(m.stopTick)

	// Explicitly stop all the tickers themselves so they don't take
	// up any more resources, and get rid of the list.
	for _, t := range m.tickers {
		t.Stop()
	}
	m.tickers = nil
}

// Tick is used to perform a single round of failure detection and gossip
// 节点故障检测和探测结果的 gossip 传播
func (m *Memberlist) probe() {
	// Track the number of indexes we've considered probing
	// numCheck 存储了本次探测尝试的次数，考虑到某些情况下被随机选中的探测节点不会被执行探测过程，因此需要重新选择
	numCheck := 0
START:
	m.nodeLock.RLock()

	// Make sure we don't wrap around infinitely
	// 若执行探测的尝试次数达到了节点数目，则直接退出，以保证我们不会无限的探测下去。
	if numCheck >= len(m.nodes) {
		m.nodeLock.RUnlock()
		return
	}

	// Handle the wrap around case
	// 若随机的探测索引值超过了节点列表的最大索引值，则先删除 dead 的节点，然后打散本地节点列表，再重新尝试探测
	if m.probeIndex >= len(m.nodes) {
		m.nodeLock.RUnlock()
		m.resetNodes()
		m.probeIndex = 0
		numCheck++
		goto START
	}

	// Determine if we should probe this node
	skip := false
	var node nodeState

	// 跳过自身节点的探测、dead 节点和 left 节点的探测
	node = *m.nodes[m.probeIndex]
	if node.Name == m.config.Name {
		skip = true
	} else if node.DeadOrLeft() {
		skip = true
	}

	// Potentially skip
	m.nodeLock.RUnlock()
	m.probeIndex++
	if skip {
		numCheck++
		goto START
	}

	// Probe the specific node
	// 真正执行探测指定节点的过程
	m.probeNode(&node)
}

// probeNodeByAddr just safely calls probeNode given only the address of the node (for tests)
func (m *Memberlist) probeNodeByAddr(addr string) {
	m.nodeLock.RLock()
	n := m.nodeMap[addr]
	m.nodeLock.RUnlock()

	m.probeNode(n)
}

// failedRemote checks the error and decides if it indicates a failure on the
// other end.
func failedRemote(err error) bool {
	switch t := err.(type) {
	case *net.OpError:
		if strings.HasPrefix(t.Net, "tcp") {
			switch t.Op {
			case "dial", "read", "write":
				return true
			}
		}
	}
	return false
}

// probeNode handles a single round of failure checking on a node.
// probeNode 对指定节点执行故障探测的过程
func (m *Memberlist) probeNode(node *nodeState) {
	defer metrics.MeasureSince([]string{"memberlist", "probeNode"}, time.Now())

	// We use our health awareness to scale the overall probe interval, so we
	// slow down if we detect problems. The ticker that calls us can handle
	// us running over the base interval, and will skip missed ticks.
	// 探测超时时间是动态设置的，同节点的 local health 成正相关，
	// 一个直观的解释是，节点的 local health 值越高，其越可能处于高负载状态，
	// 因此，为了顺利接收到其他成员反馈给他的消息，他需要给与目标成员更多的响应时间。
	probeInterval := m.awareness.ScaleTimeout(m.config.ProbeInterval)
	if probeInterval > m.config.ProbeInterval {
		metrics.IncrCounter([]string{"memberlist", "degraded", "probe"}, 1)
	}

	// Prepare a ping message and setup an ack handler.
	// 构建一个 ping 消息，以及设置消息被 ack 的处理器
	selfAddr, selfPort := m.getAdvertise()
	ping := ping{
		SeqNo:      m.nextSeqNo(),
		Node:       node.Name,
		SourceAddr: selfAddr,
		SourcePort: selfPort,
		SourceNode: m.config.Name,
	}
	ackCh := make(chan ackMessage, m.config.IndirectChecks+1)
	nackCh := make(chan struct{}, m.config.IndirectChecks+1)
	m.setProbeChannels(ping.SeqNo, ackCh, nackCh, probeInterval)

	// Mark the sent time here, which should be after any pre-processing but
	// before system calls to do the actual send. This probably over-reports
	// a bit, but it's the best we can do. We had originally put this right
	// after the I/O, but that would sometimes give negative RTT measurements
	// which was not desirable.
	sent := time.Now()

	// Send a ping to the node. If this node looks like it's suspect or dead,
	// also tack on a suspect message so that it has a chance to refute as
	// soon as possible.
	// 记录 ping 消息的开始发送时间，以及 ack 消息的超时时间戳
	deadline := sent.Add(probeInterval)
	addr := node.Address()

	// Arrange for our self-awareness to get updated.
	var awarenessDelta int
	defer func() {
		m.awareness.ApplyDelta(awarenessDelta)
	}()
	// 若节点处于 Alive 状态，则向其发送一个 ping 消息，且此基于 udp 的 pingMsg 会通过 piggyback 操作发送出去。
	if node.State == StateAlive {
		if err := m.encodeAndSendMsg(node.FullAddress(), pingMsg, &ping); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to send ping: %s", err)
			if failedRemote(err) {
				goto HANDLE_REMOTE_FAILURE
			} else {
				return
			}
		}
	} else {
		// 否则，即节点不处于 Alive 状态，则以 ping 消息和 suspect 消息构建一个 compound 消息，然后直接通过 udp 发送出去。
		// 直接发送出去的原因是，考虑到目标节点可能并非处于不健康的状态，因此需要尽快纠正此现象，而不是使用基于 gossip 的消息广播的方式，
		// 该方法需要更多的时间才能使得消息被目标节点接收。
		var msgs [][]byte
		if buf, err := encode(pingMsg, &ping); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to encode ping message: %s", err)
			return
		} else {
			msgs = append(msgs, buf.Bytes())
		}
		s := suspect{Incarnation: node.Incarnation, Node: node.Name, From: m.config.Name}
		if buf, err := encode(suspectMsg, &s); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to encode suspect message: %s", err)
			return
		} else {
			msgs = append(msgs, buf.Bytes())
		}

		compound := makeCompoundMessage(msgs)
		if err := m.rawSendMsgPacket(node.FullAddress(), &node.Node, compound.Bytes()); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to send compound ping and suspect message to %s: %s", addr, err)
			if failedRemote(err) {
				goto HANDLE_REMOTE_FAILURE
			} else {
				return
			}
		}
	}

	// Arrange for our self-awareness to get updated. At this point we've
	// sent the ping, so any return statement means the probe succeeded
	// which will improve our health until we get to the failure scenarios
	// at the end of this function, which will alter this delta variable
	// accordingly.
	// 更新自身的 awareness 值。考虑到，此时节点已成功发送了 ping 消息，因此后续任何的中断都表明探测动作
	// 已经成功执行，这表明节点自身是健康的，因此，需要更新（提升）节点的健康值。
	awarenessDelta = -1

	// Wait for response or round-trip-time.
	// 等待目标节点响应或者定时器超时，
	// 若目标探测节点成功返回 ack，则在回调上层应用的 Complete hook 后，直接退出后续处理流程。
	// 否则若节点响应 nack 或定时器超时后，则继续后续的间接探测过程。
	select {
	case v := <-ackCh:
		if v.Complete == true {
			if m.config.Ping != nil {
				rtt := v.Timestamp.Sub(sent)
				m.config.Ping.NotifyPingComplete(&node.Node, rtt, v.Payload)
			}
			return
		}

		// As an edge case, if we get a timeout, we need to re-enqueue it
		// here to break out of the select below.
		// 尽管节点响应超时，仍然需要将该消息入队，不然后续取不出来。
		if v.Complete == false {
			ackCh <- v
		}
	case <-time.After(m.config.ProbeTimeout):
		// Note that we don't scale this timeout based on awareness and
		// the health score. That's because we don't really expect waiting
		// longer to help get UDP through. Since health does extend the
		// probe interval it will give the TCP fallback more time, which
		// is more active in dealing with lost packets, and it gives more
		// time to wait for indirect acks/nacks.
		// 注意：在超时后，不会加大超时时间然后重试。
		// 这是因为我们不期望仅通过一次 udp 尝试就使得探测成功。
		// 相反，后续我们将采用 tcp 的方式来继续尝试探测。因为，对于 tcp 的探测方式，
		// 基于节点的健康度可以增加更大的超时时限，这能更好的处理由于网络波动而丢包的情况，
		// 同时也给予我们更多的时间来等待目标节点的 ack 或者 nack 消息。
		m.logger.Printf("[DEBUG] memberlist: Failed ping: %s (timeout reached)", node.Name)
	}

HANDLE_REMOTE_FAILURE:
	// 或是探测失败是由远程目标节点导致的，则开始执行间接探测流程。
	// 首先从本地集群成员视图中选择 k 个成员，要求被选中的成员不能是自身，且必须处于 alive 状态。
	// Get some random live nodes.
	m.nodeLock.RLock()
	kNodes := kRandomNodes(m.config.IndirectChecks, m.nodes, func(n *nodeState) bool {
		return n.Name == m.config.Name ||
			n.Name == node.Name ||
			n.State != StateAlive
	})
	m.nodeLock.RUnlock()

	// Attempt an indirect ping.
	// 尝试执行一个间接探测，即向他们发送基于 udp 的 indirectPing 消息。
	expectedNacks := 0
	selfAddr, selfPort = m.getAdvertise()
	ind := indirectPingReq{
		SeqNo:      ping.SeqNo,
		Target:     node.Addr,
		Port:       node.Port,
		Node:       node.Name,
		SourceAddr: selfAddr,
		SourcePort: selfPort,
		SourceNode: m.config.Name,
	}
	for _, peer := range kNodes {
		// We only expect nack to be sent from peers who understand
		// version 4 of the protocol.
		if ind.Nack = peer.PMax >= 4; ind.Nack {
			expectedNacks++
		}

		if err := m.encodeAndSendMsg(peer.FullAddress(), indirectPingMsg, &ind); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to send indirect ping: %s", err)
		}
	}

	// Also make an attempt to contact the node directly over TCP. This
	// helps prevent confused clients who get isolated from UDP traffic
	// but can still speak TCP (which also means they can possibly report
	// misinformation to other nodes via anti-entropy), avoiding flapping in
	// the cluster.
	//
	// This is a little unusual because we will attempt a TCP ping to any
	// member who understands version 3 of the protocol, regardless of
	// which protocol version we are speaking. That's why we've included a
	// config option to turn this off if desired.
	// 上面提到，当 udp 直接探测失败时，会转向使用 tcp 来重试。
	// 这当对端的网络被错误配置为禁止 udp 包，而允许 tcp 包时会有效。
	fallbackCh := make(chan bool, 1)

	// 只要没有配置禁止使用 tcp 探测，就转向使用 tcp 向目标节点发送 ping
	disableTcpPings := m.config.DisableTcpPings ||
		(m.config.DisableTcpPingsForNode != nil && m.config.DisableTcpPingsForNode(node.Name))
	if (!disableTcpPings) && (node.PMax >= 3) {
		go func() {
			defer close(fallbackCh)
			didContact, err := m.sendPingAndWaitForAck(node.FullAddress(), ping, deadline)
			if err != nil {
				m.logger.Printf("[ERR] memberlist: Failed fallback ping: %s", err)
			} else {
				fallbackCh <- didContact
			}
		}()
	} else {
		close(fallbackCh)
	}

	// Wait for the acks or timeout. Note that we don't check the fallback
	// channel here because we want to issue a warning below if that's the
	// *only* way we hear back from the peer, so we have to let this time
	// out first to allow the normal UDP-based acks to come in.
	// 等待 udp 探测请求响应或者超时（v.Complete = false）。
	// 这里没有检测 tcp 请求的响应，因为，即使 tcp 响应成功，对端也属于不太正常的情况。
	// 因此需要给出 warning。
	select {
	case v := <-ackCh:
		if v.Complete == true {
			return
		}
	}

	// Finally, poll the fallback channel. The timeouts are set such that
	// the channel will have something or be closed without having to wait
	// any additional time here.
	// 最后，轮询等从 fallback 通道中读取响应，或者超时返回。
	for didContact := range fallbackCh {
		if didContact {
			m.logger.Printf("[WARN] memberlist: Was able to connect to %s but other probes failed, network may be misconfigured", node.Name)
			return
		}
	}

	// Update our self-awareness based on the results of this failed probe.
	// If we don't have peers who will send nacks then we penalize for any
	// failed probe as a simple health metric. If we do have peers to nack
	// verify, then we can use that as a more sophisticated measure of self-
	// health because we assume them to be working, and they can help us
	// decide if the probed node was really dead or if it was something wrong
	// with ourselves.
	// 当探测失败时，更新自身的 awareness 值。
	// 需要注意的是，间接探测返回的 nack 数目同 awareness 值的更新密切相关。
	awarenessDelta = 0
	if expectedNacks > 0 {
		if nackCount := len(nackCh); nackCount < expectedNacks {
			awarenessDelta += (expectedNacks - nackCount)
		}
	} else {
		awarenessDelta += 1
	}

	// No acks received from target, suspect it as failed.
	// 若通过 tcp 也探测失败，则说明目标节点可能发生故障，
	// 因此，首先更新节点自身的 local health 值，然后进入到怀疑节点（suspectNode）的操作流程
	m.logger.Printf("[INFO] memberlist: Suspect %s has failed, no acks received", node.Name)
	s := suspect{Incarnation: node.Incarnation, Node: node.Name, From: m.config.Name}
	m.suspectNode(&s)
}

// Ping initiates a ping to the node with the specified name.
func (m *Memberlist) Ping(node string, addr net.Addr) (time.Duration, error) {
	// Prepare a ping message and setup an ack handler.
	selfAddr, selfPort := m.getAdvertise()
	ping := ping{
		SeqNo:      m.nextSeqNo(),
		Node:       node,
		SourceAddr: selfAddr,
		SourcePort: selfPort,
		SourceNode: m.config.Name,
	}
	ackCh := make(chan ackMessage, m.config.IndirectChecks+1)
	m.setProbeChannels(ping.SeqNo, ackCh, nil, m.config.ProbeInterval)

	a := Address{Addr: addr.String(), Name: node}

	// Send a ping to the node.
	if err := m.encodeAndSendMsg(a, pingMsg, &ping); err != nil {
		return 0, err
	}

	// Mark the sent time here, which should be after any pre-processing and
	// system calls to do the actual send. This probably under-reports a bit,
	// but it's the best we can do.
	sent := time.Now()

	// Wait for response or timeout.
	select {
	case v := <-ackCh:
		if v.Complete == true {
			return v.Timestamp.Sub(sent), nil
		}
	case <-time.After(m.config.ProbeTimeout):
		// Timeout, return an error below.
	}

	m.logger.Printf("[DEBUG] memberlist: Failed UDP ping: %v (timeout reached)", node)
	return 0, NoPingResponseError{ping.Node}
}

// resetNodes is used when the tick wraps around. It will reap the
// dead nodes and shuffle the node list.
// resetNodes 回收节点本地视图中的 dead 节点，并打散节点本地视图集的节点索引
func (m *Memberlist) resetNodes() {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()

	// Move dead nodes, but respect gossip to the dead interval
	// moveDeadNodes 将本地视图中的 dead 节点（且 gossip 时间小于状态变更的时间）移动节点列表的末尾，以便于后续的截取操作
	deadIdx := moveDeadNodes(m.nodes, m.config.GossipToTheDeadTime)

	// Deregister the dead nodes
	// 将 daed 节点在本地集群成员视图中删除
	for i := deadIdx; i < len(m.nodes); i++ {
		delete(m.nodeMap, m.nodes[i].Name)
		m.nodes[i] = nil
	}

	// Trim the nodes to exclude the dead nodes
	// 将 dead 节点从列表末端中移除
	m.nodes = m.nodes[0:deadIdx]

	// Update numNodes after we've trimmed the dead nodes
	// 更新集群中节点数目
	atomic.StoreUint32(&m.numNodes, uint32(deadIdx))

	// Shuffle live nodes
	// 打散节点保存的本地集群节点列表
	shuffleNodes(m.nodes)
}

// gossip is invoked every GossipInterval period to broadcast our gossip
// messages to a few random nodes.
// gossip 函数用于定期地广播 gossip 消息给随机中随机的 k 个节点
func (m *Memberlist) gossip() {
	defer metrics.MeasureSince([]string{"memberlist", "gossip"}, time.Now())

	// Get some random live, suspect, or recently dead nodes
	// 随机选择节点时，只选择 alive、suspect 以及部分 dead 节点。
	m.nodeLock.RLock()
	kNodes := kRandomNodes(m.config.GossipNodes, m.nodes, func(n *nodeState) bool {
		if n.Name == m.config.Name {
			return true
		}

		switch n.State {
		case StateAlive, StateSuspect:
			return false

		case StateDead:
			return time.Since(n.StateChange) > m.config.GossipToTheDeadTime

		default:
			return true
		}
	})
	m.nodeLock.RUnlock()

	// Compute the bytes available
	bytesAvail := m.config.UDPBufferSize - compoundHeaderOverhead
	if m.config.EncryptionEnabled() {
		bytesAvail -= encryptOverhead(m.encryptionVersion())
	}

	// 从广播消息队列中取出若干消息，以构成 compound 消息，然后依次向他们发送此 compound 消息。
	for _, node := range kNodes {
		// Get any pending broadcasts
		// 从缓冲队列中选择总容量固定的消息集合
		msgs := m.getBroadcasts(compoundOverhead, bytesAvail)
		if len(msgs) == 0 {
			return
		}

		addr := node.Address()
		if len(msgs) == 1 {
			// Send single message as is
			if err := m.rawSendMsgPacket(node.FullAddress(), &node, msgs[0]); err != nil {
				m.logger.Printf("[ERR] memberlist: Failed to send gossip to %s: %s", addr, err)
			}
		} else {
			// Otherwise create and send a compound message
			compound := makeCompoundMessage(msgs)
			if err := m.rawSendMsgPacket(node.FullAddress(), &node, compound.Bytes()); err != nil {
				m.logger.Printf("[ERR] memberlist: Failed to send gossip to %s: %s", addr, err)
			}
		}
	}
}

// pushPull is invoked periodically to randomly perform a complete state
// exchange. Used to ensure a high level of convergence, but is also
// reasonably expensive as the entire state of this node is exchanged
// with the other node.
// pushPull 执行一个节点全量状态交换的操作以交换节点同集群其他成员的集群节点成员视图状态。
// 该操作比较耗时。
// 即随机选择 k 个节点进行节点之间的全量状态交换。
// 此操作的目的是加速集群状态收敛，不必使用基于 gossip 的消息传播方式，
// 这在新节点加入集群，或者整个集群出现网络分区恢复的场景尤为有效。
// 此操作的一个代价是网络带宽，
// 因此，显然此操作不能过于频繁，特别是在集群规模较大的情况
func (m *Memberlist) pushPull() {
	// Get a random live node
	m.nodeLock.RLock()
	nodes := kRandomNodes(1, m.nodes, func(n *nodeState) bool {
		return n.Name == m.config.Name ||
			n.State != StateAlive
	})
	m.nodeLock.RUnlock()

	// If no nodes, bail
	if len(nodes) == 0 {
		return
	}
	node := nodes[0]

	// Attempt a push pull
	if err := m.pushPullNode(node.FullAddress(), false); err != nil {
		m.logger.Printf("[ERR] memberlist: Push/Pull with %s failed: %s", node.Name, err)
	}
}

// pushPullNode does a complete state exchange with a specific node.
func (m *Memberlist) pushPullNode(a Address, join bool) error {
	defer metrics.MeasureSince([]string{"memberlist", "pushPullNode"}, time.Now())

	// Attempt to send and receive with the node
	// 首先，针对选中的节点执行 push->pull 操作。
	// push 和 pull 操作都基于 tcp 连接
	remote, userState, err := m.sendAndReceiveState(a, join)
	if err != nil {
		return err
	}

	// 执行节点状态数据的合并操作
	if err := m.mergeRemoteState(join, remote, userState); err != nil {
		return err
	}
	return nil
}

// verifyProtocol verifies that all the remote nodes can speak with our
// nodes and vice versa on both the core protocol as well as the
// delegate protocol level.
//
// The verification works by finding the maximum minimum and
// minimum maximum understood protocol and delegate versions. In other words,
// it finds the common denominator of protocol and delegate version ranges
// for the entire cluster.
//
// After this, it goes through the entire cluster (local and remote) and
// verifies that everyone's speaking protocol versions satisfy this range.
// If this passes, it means that every node can understand each other.
func (m *Memberlist) verifyProtocol(remote []pushNodeState) error {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	// Maximum minimum understood and minimum maximum understood for both
	// the protocol and delegate versions. We use this to verify everyone
	// can be understood.
	var maxpmin, minpmax uint8
	var maxdmin, mindmax uint8
	minpmax = math.MaxUint8
	mindmax = math.MaxUint8

	for _, rn := range remote {
		// If the node isn't alive, then skip it
		if rn.State != StateAlive {
			continue
		}

		// Skip nodes that don't have versions set, it just means
		// their version is zero.
		if len(rn.Vsn) == 0 {
			continue
		}

		if rn.Vsn[0] > maxpmin {
			maxpmin = rn.Vsn[0]
		}

		if rn.Vsn[1] < minpmax {
			minpmax = rn.Vsn[1]
		}

		if rn.Vsn[3] > maxdmin {
			maxdmin = rn.Vsn[3]
		}

		if rn.Vsn[4] < mindmax {
			mindmax = rn.Vsn[4]
		}
	}

	for _, n := range m.nodes {
		// Ignore non-alive nodes
		if n.State != StateAlive {
			continue
		}

		if n.PMin > maxpmin {
			maxpmin = n.PMin
		}

		if n.PMax < minpmax {
			minpmax = n.PMax
		}

		if n.DMin > maxdmin {
			maxdmin = n.DMin
		}

		if n.DMax < mindmax {
			mindmax = n.DMax
		}
	}

	// Now that we definitively know the minimum and maximum understood
	// version that satisfies the whole cluster, we verify that every
	// node in the cluster satisifies this.
	for _, n := range remote {
		var nPCur, nDCur uint8
		if len(n.Vsn) > 0 {
			nPCur = n.Vsn[2]
			nDCur = n.Vsn[5]
		}

		if nPCur < maxpmin || nPCur > minpmax {
			return fmt.Errorf(
				"Node '%s' protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nPCur, maxpmin, minpmax)
		}

		if nDCur < maxdmin || nDCur > mindmax {
			return fmt.Errorf(
				"Node '%s' delegate protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nDCur, maxdmin, mindmax)
		}
	}

	for _, n := range m.nodes {
		nPCur := n.PCur
		nDCur := n.DCur

		if nPCur < maxpmin || nPCur > minpmax {
			return fmt.Errorf(
				"Node '%s' protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nPCur, maxpmin, minpmax)
		}

		if nDCur < maxdmin || nDCur > mindmax {
			return fmt.Errorf(
				"Node '%s' delegate protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nDCur, maxdmin, mindmax)
		}
	}

	return nil
}

// nextSeqNo returns a usable sequence number in a thread safe way
func (m *Memberlist) nextSeqNo() uint32 {
	return atomic.AddUint32(&m.sequenceNum, 1)
}

// nextIncarnation returns the next incarnation number in a thread safe way
func (m *Memberlist) nextIncarnation() uint32 {
	return atomic.AddUint32(&m.incarnation, 1)
}

// skipIncarnation adds the positive offset to the incarnation number.
func (m *Memberlist) skipIncarnation(offset uint32) uint32 {
	return atomic.AddUint32(&m.incarnation, offset)
}

// estNumNodes is used to get the current estimate of the number of nodes
func (m *Memberlist) estNumNodes() int {
	return int(atomic.LoadUint32(&m.numNodes))
}

type ackMessage struct {
	Complete  bool
	Payload   []byte
	Timestamp time.Time
}

// setProbeChannels is used to attach the ackCh to receive a message when an ack
// with a given sequence number is received. The `complete` field of the message
// will be false on timeout. Any nack messages will cause an empty struct to be
// passed to the nackCh, which can be nil if not needed.
// setProbeChannels 设置 ping 消息被 ack 或者 nack 的处理器，同时，当超时未回复时，则删除对应的处理器
func (m *Memberlist) setProbeChannels(seqNo uint32, ackCh chan ackMessage, nackCh chan struct{}, timeout time.Duration) {
	// Create handler functions for acks and nacks
	ackFn := func(payload []byte, timestamp time.Time) {
		select {
		case ackCh <- ackMessage{true, payload, timestamp}:
		default:
		}
	}
	nackFn := func() {
		select {
		case nackCh <- struct{}{}:
		default:
		}
	}

	// Add the handlers
	ah := &ackHandler{ackFn, nackFn, nil}
	m.ackLock.Lock()
	m.ackHandlers[seqNo] = ah
	m.ackLock.Unlock()

	// Setup a reaping routing
	ah.timer = time.AfterFunc(timeout, func() {
		m.ackLock.Lock()
		delete(m.ackHandlers, seqNo)
		m.ackLock.Unlock()
		select {
		case ackCh <- ackMessage{false, nil, time.Now()}:
		default:
		}
	})
}

// setAckHandler is used to attach a handler to be invoked when an ack with a
// given sequence number is received. If a timeout is reached, the handler is
// deleted. This is used for indirect pings so does not configure a function
// for nacks.
func (m *Memberlist) setAckHandler(seqNo uint32, ackFn func([]byte, time.Time), timeout time.Duration) {
	// Add the handler
	ah := &ackHandler{ackFn, nil, nil}
	m.ackLock.Lock()
	m.ackHandlers[seqNo] = ah
	m.ackLock.Unlock()

	// Setup a reaping routing
	ah.timer = time.AfterFunc(timeout, func() {
		m.ackLock.Lock()
		delete(m.ackHandlers, seqNo)
		m.ackLock.Unlock()
	})
}

// Invokes an ack handler if any is associated, and reaps the handler immediately
func (m *Memberlist) invokeAckHandler(ack ackResp, timestamp time.Time) {
	m.ackLock.Lock()
	ah, ok := m.ackHandlers[ack.SeqNo]
	delete(m.ackHandlers, ack.SeqNo)
	m.ackLock.Unlock()
	if !ok {
		return
	}
	ah.timer.Stop()
	ah.ackFn(ack.Payload, timestamp)
}

// Invokes nack handler if any is associated.
func (m *Memberlist) invokeNackHandler(nack nackResp) {
	m.ackLock.Lock()
	ah, ok := m.ackHandlers[nack.SeqNo]
	m.ackLock.Unlock()
	if !ok || ah.nackFn == nil {
		return
	}
	ah.nackFn()
}

// refute gossips an alive message in response to incoming information that we
// are suspect or dead. It will make sure the incarnation number beats the given
// accusedInc value, or you can supply 0 to just get the next incarnation number.
// This alters the node state that's passed in so this MUST be called while the
// nodeLock is held.
// refute 通过广播一条 alive 消息来驳斥其它节点针对自身的 suspect 或者 dead 消息。
func (m *Memberlist) refute(me *nodeState, accusedInc uint32) {
	// Make sure the incarnation number beats the accusation.
	// 首先递增自身的的 incarnation，以保证该值大于其它节点为自己保存的该值，否则将不能驳斥成功。
	inc := m.nextIncarnation()
	// 若其它节点为自己保存的 incarnation 仍旧大于递增后的值，则进一步增加 incarnation 直至大于它。
	if accusedInc >= inc {
		inc = m.skipIncarnation(accusedInc - inc + 1)
	}
	me.Incarnation = inc

	// Decrease our health because we are being asked to refute a problem.
	// 减少自己的 awareness 值，考虑到其它节点认为自己是处于 suspect 或者  dead 状态，但实际上自己并没有处于该状态，
	// 因此可能是自己的
	m.awareness.ApplyDelta(1)

	// Format and broadcast an alive message.
	a := alive{
		Incarnation: inc,
		Node:        me.Name,
		Addr:        me.Addr,
		Port:        me.Port,
		Meta:        me.Meta,
		Vsn: []uint8{
			me.PMin, me.PMax, me.PCur,
			me.DMin, me.DMax, me.DCur,
		},
	}
	m.encodeAndBroadcast(me.Addr.String(), aliveMsg, a)
}

// aliveNode is invoked by the network layer when we get a message about a
// live node.
// alive 消息的处理逻辑。
func (m *Memberlist) aliveNode(a *alive, notify chan struct{}, bootstrap bool) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[a.Node]

	// It is possible that during a Leave(), there is already an aliveMsg
	// in-queue to be processed but blocked by the locks above. If we let
	// that aliveMsg process, it'll cause us to re-join the cluster. This
	// ensures that we don't.
	// 当节点自身主动离开集群的同时，存在一条 alive 消息未被此处理，
	// 则此时应该进一步检测，若属于此情况，则应该直接返回。
	if m.hasLeft() && a.Node == m.config.Name {
		return
	}

	// 协议兼容性检查
	if len(a.Vsn) >= 3 {
		pMin := a.Vsn[0]
		pMax := a.Vsn[1]
		pCur := a.Vsn[2]
		if pMin == 0 || pMax == 0 || pMin > pMax {
			m.logger.Printf("[WARN] memberlist: Ignoring an alive message for '%s' (%v:%d) because protocol version(s) are wrong: %d <= %d <= %d should be >0", a.Node, net.IP(a.Addr), a.Port, pMin, pCur, pMax)
			return
		}
	}

	// Invoke the Alive delegate if any. This can be used to filter out
	// alive messages based on custom logic. For example, using a cluster name.
	// Using a merge delegate is not enough, as it is possible for passive
	// cluster merging to still occur.
	// 调用上层应用的 alive hook 处理器。这可基于自定义的逻辑来过滤 alive 消息。
	if m.config.Alive != nil {
		if len(a.Vsn) < 6 {
			m.logger.Printf("[WARN] memberlist: ignoring alive message for '%s' (%v:%d) because Vsn is not present",
				a.Node, net.IP(a.Addr), a.Port)
			return
		}
		node := &Node{
			Name: a.Node,
			Addr: a.Addr,
			Port: a.Port,
			Meta: a.Meta,
			PMin: a.Vsn[0],
			PMax: a.Vsn[1],
			PCur: a.Vsn[2],
			DMin: a.Vsn[3],
			DMax: a.Vsn[4],
			DCur: a.Vsn[5],
		}
		if err := m.config.Alive.NotifyAlive(node); err != nil {
			m.logger.Printf("[WARN] memberlist: ignoring alive message for '%s': %s",
				a.Node, err)
			return
		}
	}

	// Check if we've never seen this node before, and if not, then
	// store this node in our node map.
	// 若将节点先前不存在于节点本地的视图中，则先构造一个节点对象并存储。
	// 然后，生成一个随机的索引位置，用于放置该节点，这可以保证节点执行随机探测时任一节点被探测的延时存在上限。
	// 最后更新当前集群中成员数目。
	var updatesNode bool
	if !ok {
		errCon := m.config.IPAllowed(a.Addr)
		if errCon != nil {
			m.logger.Printf("[WARN] memberlist: Rejected node %s (%v): %s", a.Node, net.IP(a.Addr), errCon)
			return
		}
		state = &nodeState{
			Node: Node{
				Name: a.Node,
				Addr: a.Addr,
				Port: a.Port,
				Meta: a.Meta,
			},
			State: StateDead,
		}
		if len(a.Vsn) > 5 {
			state.PMin = a.Vsn[0]
			state.PMax = a.Vsn[1]
			state.PCur = a.Vsn[2]
			state.DMin = a.Vsn[3]
			state.DMax = a.Vsn[4]
			state.DCur = a.Vsn[5]
		}

		// Add to map
		m.nodeMap[a.Node] = state

		// Get a random offset. This is important to ensure
		// the failure detection bound is low on average. If all
		// nodes did an append, failure detection bound would be
		// very high.
		n := len(m.nodes)
		offset := randomOffset(n)

		// Add at the end and swap with the node at the offset
		m.nodes = append(m.nodes, state)
		m.nodes[offset], m.nodes[n] = m.nodes[n], m.nodes[offset]

		// Update numNodes after we've added a new node
		atomic.AddUint32(&m.numNodes, 1)
	} else {
		// 若节点已存在于节点本地集群成员视图中。
		// 则进一步判断 alive 消息中存在的节点元信息（如 ip 地址或端口）是否和本地保存的对应节点的元信息冲突，
		// 若产生冲突，则需进一步判断节点是否允许更新其元信息，
		// 显然，只有节点处于 left 或者 dead 状态，且协议被配置为允许节点回收（即节点的名称重复使用必须间隔指定时间），
		// 节点才会更新 alive 消息中包含的节点在本地存储的元信息和状态，
		// 相反若配置为不允许更新节点状态，则回调上层应用的 Conflict hook 处理器，直接中止后续处理流程。
		// Check if this address is different than the existing node unless the old node is dead.
		if !bytes.Equal([]byte(state.Addr), a.Addr) || state.Port != a.Port {
			errCon := m.config.IPAllowed(a.Addr)
			if errCon != nil {
				m.logger.Printf("[WARN] memberlist: Rejected IP update from %v to %v for node %s: %s", a.Node, state.Addr, net.IP(a.Addr), errCon)
				return
			}
			// If DeadNodeReclaimTime is configured, check if enough time has elapsed since the node died.
			canReclaim := (m.config.DeadNodeReclaimTime > 0 &&
				time.Since(state.StateChange) > m.config.DeadNodeReclaimTime)

			// Allow the address to be updated if a dead node is being replaced.
			if state.State == StateLeft || (state.State == StateDead && canReclaim) {
				m.logger.Printf("[INFO] memberlist: Updating address for left or failed node %s from %v:%d to %v:%d",
					state.Name, state.Addr, state.Port, net.IP(a.Addr), a.Port)
				updatesNode = true
			} else {
				m.logger.Printf("[ERR] memberlist: Conflicting address for %s. Mine: %v:%d Theirs: %v:%d Old state: %v",
					state.Name, state.Addr, state.Port, net.IP(a.Addr), a.Port, state.State)

				// Inform the conflict delegate if provided
				if m.config.Conflict != nil {
					other := Node{
						Name: a.Node,
						Addr: a.Addr,
						Port: a.Port,
						Meta: a.Meta,
					}
					m.config.Conflict.NotifyConflict(&state.Node, &other)
				}
				return
			}
		}
	}

	// Bail if the incarnation number is older, and this is not about us
	// 当节点的 incarnation 的值小于本节点为其存在的值，并且目标节点并非自身，同时也并未执行节点信息变更时，则直接退出。
	isLocalNode := state.Name == m.config.Name
	if a.Incarnation <= state.Incarnation && !isLocalNode && !updatesNode {
		return
	}

	// Bail if strictly less and this is about us
	// 当节点的 incarnation 的值小于本节点为其存在的值，并且目标节点即为自身，同样直接退出。
	if a.Incarnation < state.Incarnation && isLocalNode {
		return
	}

	// Clear out any suspicion timer that may be in effect.
	// 先清除节点的 suspect 定时器，若存在的话。因为该节点收到了目标节点的 alive 消息。
	delete(m.nodeTimers, a.Node)

	// Store the old state and meta data
	oldState := state.State
	oldMeta := state.Meta

	// If this is us we need to refute, otherwise re-broadcast
	// 若发现此 alive 消息正是针对节点自身，且并不是节点自身在启动时加入集群时发出的，
	// 则节点可能需要执行 refute 操作，即向集群中广播 alive 消息，以公告自己仍然处于存活状态。
	if !bootstrap && isLocalNode {
		// Compute the version vector
		versions := []uint8{
			state.PMin, state.PMax, state.PCur,
			state.DMin, state.DMax, state.DCur,
		}

		// If the Incarnation is the same, we need special handling, since it
		// possible for the following situation to happen:
		// 1) Start with configuration C, join cluster
		// 2) Hard fail / Kill / Shutdown
		// 3) Restart with configuration C', join cluster
		//
		// In this case, other nodes and the local node see the same incarnation,
		// but the values may not be the same. For this reason, we always
		// need to do an equality check for this Incarnation. In most cases,
		// we just ignore, but we may need to refute.
		//
		if a.Incarnation == state.Incarnation &&
			bytes.Equal(a.Meta, state.Meta) &&
			bytes.Equal(a.Vsn, versions) {
			return
		}
		m.refute(state, a.Incarnation)
		m.logger.Printf("[WARN] memberlist: Refuting an alive message for '%s' (%v:%d) meta:(%v VS %v), vsn:(%v VS %v)", a.Node, net.IP(a.Addr), a.Port, a.Meta, state.Meta, a.Vsn, versions)
	} else {
		// 相反，若发现此 aliveMsg 同自身无关，或者即使此消息同自身相关，
		// 但也并非在节点启动加入集群时发出的，此时直接将此 aliveMsg 广播到集群。
		// 最后更新本节点为目标节点存储的元信息，如 incarnation 值，状态更新时间等。
		m.encodeBroadcastNotify(a.Node, aliveMsg, a, notify)

		// Update protocol versions if it arrived
		if len(a.Vsn) > 0 {
			state.PMin = a.Vsn[0]
			state.PMax = a.Vsn[1]
			state.PCur = a.Vsn[2]
			state.DMin = a.Vsn[3]
			state.DMax = a.Vsn[4]
			state.DCur = a.Vsn[5]
		}

		// Update the state and incarnation number
		state.Incarnation = a.Incarnation
		state.Meta = a.Meta
		state.Addr = a.Addr
		state.Port = a.Port
		if state.State != StateAlive {
			state.State = StateAlive
			state.StateChange = time.Now()
		}
	}

	// Update metrics
	metrics.IncrCounter([]string{"memberlist", "msg", "alive"}, 1)

	// Notify the delegate of any relevant updates
	// 若上层应用定义了节点状态变化的 hook，则需要回调它们。
	// 节点状态变化分为节点的存活状态变化：  dead/left -> alive，
	// 以及节点的元信息发生变化。
	if m.config.Events != nil {
		if oldState == StateDead || oldState == StateLeft {
			// if Dead/Left -> Alive, notify of join
			m.config.Events.NotifyJoin(&state.Node)

		} else if !bytes.Equal(oldMeta, state.Meta) {
			// if Meta changed, trigger an update notification
			m.config.Events.NotifyUpdate(&state.Node)
		}
	}
}

// suspectNode is invoked by the network layer when we get a message
// about a suspect node
func (m *Memberlist) suspectNode(s *suspect) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[s.Node]

	// If we've never heard about this node before, ignore it
	// 若被 suspect 的节点平不存在于当前节点的集群节点列表视图中，则忽略该消息。
	// 说明该消息可能已被处理过。
	if !ok {
		return
	}

	// Ignore old incarnation numbers
	// 类似地，若被 suspect 的节点的 incarnation 值小于当前节点为该 suspect 保存的 incarnation 值，同样忽略该消息。
	// 说明该消息已经过时了。
	if s.Incarnation < state.Incarnation {
		return
	}

	// See if there's a suspicion timer we can confirm. If the info is new
	// to us we will go ahead and re-gossip it. This allows for multiple
	// independent confirmations to flow even when a node probes a node
	// that's already suspect.
	// 若当前节点已为目标节点保存了对应的 suspect timer，则当收到其它节点的针对目标节点的 suspect 消息时，
	// 执行 confirm 动作，表明进一步“肯定”目标节点处于 dead 状态。
	// 然后将此 suspect 发送到需要被广播的消息缓存队列中，随后会被广播出去。
	if timer, ok := m.nodeTimers[s.Node]; ok {
		if timer.Confirm(s.From) {
			m.encodeAndBroadcast(s.Node, suspectMsg, s)
		}
		return
	}

	// Ignore non-alive nodes
	// 若当前节点已非 alive 状态，则忽略它。因为在当前节点看来，该节点已经处于 suspect 或 dead 的状态，不需要后续处理。
	if state.State != StateAlive {
		return
	}

	// If this is us we need to refute, otherwise re-broadcast
	// 若恰好发现目标节点就是当前节点自身，则显然，自身还是存活的，因此需要立即发送一条 refute 消息以驳斥该 suspect 消息。
	// 否则，将该 suspect 消息发送到需要被广播的消息缓存队列中，随后会被广播出去。
	if state.Name == m.config.Name {
		m.refute(state, s.Incarnation)
		m.logger.Printf("[WARN] memberlist: Refuting a suspect message (from: %s)", s.From)
		return // Do not mark ourself suspect
	} else {
		m.encodeAndBroadcast(s.Node, suspectMsg, s)
	}

	// Update metrics
	metrics.IncrCounter([]string{"memberlist", "msg", "suspect"}, 1)

	// Update the state
	// 更新当前节点为目标节点保存的 incarnation 值，目标节点的状态、目标节点状态更新时间
	state.Incarnation = s.Incarnation
	state.State = StateSuspect
	changeTime := time.Now()
	state.StateChange = changeTime

	// Setup a suspicion timer. Given that we don't have any known phase
	// relationship with our peers, we set up k such that we hit the nominal
	// timeout two probe intervals short of what we expect given the suspicion
	// multiplier.
	// 设置一个 suspect 计时器。考虑到当前节点未和其它节点有任何联系，
	// 因此初始设置的超时时间比我们预期的超时时间短两个探测间隔(给定怀疑乘数)
	k := m.config.SuspicionMult - 2

	// If there aren't enough nodes to give the expected confirmations, just
	// set k to 0 to say that we don't expect any. Note we subtract 2 from n
	// here to take out ourselves and the node being probed.
	// 但若发现集群中的节点（除自身和目标节点处）还小于 k，则直接将 k 设置为 0，以表明我们不期待任何的针对目标节点的 Confirm 操作。
	n := m.estNumNodes()
	if n-2 < k {
		k = 0
	}

	// Compute the timeouts based on the size of the cluster.
	// 基于集群的大小以及其它超时参数来计算 suspect 定时器的超时时限的上下限。
	min := suspicionTimeout(m.config.SuspicionMult, n, m.config.ProbeInterval)
	max := time.Duration(m.config.SuspicionMaxTimeoutMult) * min
	// 构建基于其它节点对目标节点的 suspect 状态进行 Confirm 操作处理完成，或者达到超时时间的处理器。
	// 此时已基本可确认目标被 suspect 节点已经处于 dead 状态了。因此，
	// 将构建一个针对目标被 suspect 的节点的 dead 消息，然后执行对应的处理流程。
	fn := func(numConfirmations int) {
		var d *dead

		m.nodeLock.Lock()
		state, ok := m.nodeMap[s.Node]
		timeout := ok && state.State == StateSuspect && state.StateChange == changeTime
		if timeout {
			d = &dead{Incarnation: state.Incarnation, Node: state.Name, From: m.config.Name}
		}
		m.nodeLock.Unlock()

		if timeout {
			if k > 0 && numConfirmations < k {
				metrics.IncrCounter([]string{"memberlist", "degraded", "timeout"}, 1)
			}

			m.logger.Printf("[INFO] memberlist: Marking %s as failed, suspect timeout reached (%d peer confirmations)",
				state.Name, numConfirmations)

			m.deadNode(d)
		}
	}
	// 为该目标节点构建 suspect 超时定时器，并保存
	m.nodeTimers[s.Node] = newSuspicion(s.From, k, min, max, fn)
}

// deadNode is invoked by the network layer when we get a message
// about a dead node
// dead 消息的处理逻辑。
func (m *Memberlist) deadNode(d *dead) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[d.Node]

	// If we've never heard about this node before, ignore it
	// 若该节点不存在于节点的本地集群成员视图中，则直接忽略它，不予处理。
	if !ok {
		return
	}

	// Ignore old incarnation numbers
	// 若该节点的 incarnation 值要小于本节点为其存在的 incarnation 值，则同样不予处理。
	if d.Incarnation < state.Incarnation {
		return
	}

	// Clear out any suspicion timer that may be in effect.
	// 否则，首先清除本节点为目标节点设置的 suspect 定时器。
	delete(m.nodeTimers, d.Node)

	// Ignore if node is already dead
	// 若目标节点已处于 dead 或 left 状态，则直接忽略本消息。
	if state.DeadOrLeft() {
		return
	}

	// Check if this is us
	// 节点会判断此 deadMsg 的目标成员是否即为自身，
	// 若发现当前的 deadMsg 确实针对的是节点自身，且节点自身仍处于存活状态（未宕机），
	// 因此，会进入驳斥怀疑的流程，即向集群广播 alive 消息，同时更新自身的 local health 值，然后返回。
	if state.Name == m.config.Name {
		// If we are not leaving we need to refute
		if !m.hasLeft() {
			m.refute(state, d.Incarnation)
			m.logger.Printf("[WARN] memberlist: Refuting a dead message (from: %s)", d.From)
			return // Do not mark ourself dead
		}

		// If we are leaving, we broadcast and wait
		// 另一种情况是，节点主动调用 Leave 离开集群，
		// 则此时可进一步广播此 dead 消息，以让其他成员尽可能快的知悉这一消息，使得集群状态收敛。
		// 同时设置一个接收 channel，一旦任意一个成员收到此 dead 消息，此节点就可以放心离开集群，
		// 否则应该在 Leave 操作中阻塞等待，直到集群成员知悉其已离开集群。
		// 然后，将节点状态标记为 Left（正常离开）。
		m.encodeBroadcastNotify(d.Node, deadMsg, d, m.leaveBroadcast)
	} else {
		m.encodeAndBroadcast(d.Node, deadMsg, d)
	}

	// Update metrics
	metrics.IncrCounter([]string{"memberlist", "msg", "dead"}, 1)

	// Update the state
	// 更新本节点为目标节点保存的 incarnation 值。
	state.Incarnation = d.Incarnation

	// If the dead message was send by the node itself, mark it is left
	// instead of dead.
	// 否则，若 dead 消息的节点即为发送该 dead 消息的节点，则说明该节点为主动离开集群。
	// 因此，需要将节点的状态标记为 Left 而并非 Dead，反之亦然。
	// 最后，更新本节点为目标节点保存的视图，比如节点状态的更新时间。
	if d.Node == d.From {
		state.State = StateLeft
	} else {
		state.State = StateDead
	}
	state.StateChange = time.Now()

	// Notify of death
	// 最后回调上层应用针对节点离开集群的事件设置的 hook。
	if m.config.Events != nil {
		m.config.Events.NotifyLeave(&state.Node)
	}
}

// mergeState is invoked by the network layer when we get a Push/Pull
// state transfer
// 当节点通过 push->pull->merge 操作收到了目标节点集合，
// 则遍历每一个远程节点，根据目标节点的状态来执行对应的操作。
// 比如，目标节点处于 alive 状态，则应该执行 alive 处理器。
func (m *Memberlist) mergeState(remote []pushNodeState) {
	for _, r := range remote {
		switch r.State {
		case StateAlive:
			a := alive{
				Incarnation: r.Incarnation,
				Node:        r.Name,
				Addr:        r.Addr,
				Port:        r.Port,
				Meta:        r.Meta,
				Vsn:         r.Vsn,
			}
			m.aliveNode(&a, nil, false)

		case StateLeft:
			d := dead{Incarnation: r.Incarnation, Node: r.Name, From: r.Name}
			m.deadNode(&d)
		// 需要注意的是，即使节点的状态为 dead，其仍然选择通过发送 suspect 消息，
		// 以给与节点驳斥怀疑的机会，而不是直接将节点标记为 Dead 并广播 dead 消息。
		case StateDead:
			// If the remote node believes a node is dead, we prefer to
			// suspect that node instead of declaring it dead instantly
			fallthrough
		case StateSuspect:
			s := suspect{Incarnation: r.Incarnation, Node: r.Name, From: m.config.Name}
			m.suspectNode(&s)
		}
	}
}
