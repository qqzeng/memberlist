package memberlist

import "time"

// PingDelegate is used to notify an observer how long it took for a ping message to
// complete a round trip.  It can also be used for writing arbitrary byte slices
// into ack messages. Note that in order to be meaningful for RTT estimates, this
// delegate does not apply to indirect pings, nor fallback pings sent over TCP.
// PingDelegate 可用于计算 RTT，也可用于上层应用为收到 ping 消息时，在响应的 ack 消息中添加任意的内容。
type PingDelegate interface {
	// AckPayload is invoked when an ack is being sent; the returned bytes will be appended to the ack
	// 在 ping 消息的响应中附带数据。
	AckPayload() []byte
	// NotifyPing is invoked when an ack for a ping is received
	// 当收到自己对对方发送的 ping 消息的回应时，会回调该接口。
	NotifyPingComplete(other *Node, rtt time.Duration, payload []byte)
}
