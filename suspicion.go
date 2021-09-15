package memberlist

import (
	"math"
	"sync/atomic"
	"time"
)

// suspicion manages the suspect timer for a node and provides an interface
// to accelerate the timeout as we get more independent confirmations that
// a node is suspect.
// suspicion 用于管理针对节点的 suspect 定时器
type suspicion struct {
	// n is the number of independent confirmations we've seen. This must
	// be updated using atomic instructions to prevent contention with the
	// timer callback.
	// n 表示当前节点收到的对目标节点的 suspect confirmation 的数目。
	n int32

	// k is the number of independent confirmations we'd like to see in
	// order to drive the timer to its minimum value.
	// k 表示当前节点期望收到的 suspect confirmation 的数目。只有满足了该条件，才将定时器设置为最小值。
	k int32

	// min is the minimum timer value.
	// min、max 分别为定时器的最小和最大超时时限值。
	min time.Duration

	// max is the maximum timer value.
	max time.Duration

	// start captures the timestamp when we began the timer. This is used
	// so we can calculate durations to feed the timer during updates in
	// a way the achieves the overall time we'd like.
	// start 表示定时器被创建启动的开始时间，以计算定时器更新期间的持续时间，最后达到我们想要的总时间。
	start time.Time

	// timer is the underlying timer that implements the timeout.
	timer *time.Timer

	// f is the function to call when the timer expires. We hold on to this
	// because there are cases where we call it directly.
	// 定时器被触发后被执行的处理器。
	timeoutFn func()

	// confirmations is a map of "from" nodes that have confirmed a given
	// node is suspect. This prevents double counting.
	// confirmations 保存了当前节点已经针对某些 suspect 节点执行了 confirm 动作。
	confirmations map[string]struct{}
}

// newSuspicion returns a timer started with the max time, and that will drive
// to the min time after seeing k or more confirmations. The from node will be
// excluded from confirmations since we might get our own suspicion message
// gossiped back to us. The minimum time will be used if no confirmations are
// called for (k <= 0).
// newSuspicion 构建一个 suspect 定时器，每收到一个针对目标节点的 confirm，则减少 max 的值，当收到 k 个确认时，则将其等于 min。
func newSuspicion(from string, k int, min time.Duration, max time.Duration, fn func(int)) *suspicion {
	s := &suspicion{
		k:             int32(k),
		min:           min,
		max:           max,
		confirmations: make(map[string]struct{}),
	}

	// Exclude the from node from any confirmations.
	// 排除目标节点的 confirm 操作
	s.confirmations[from] = struct{}{}

	// Pass the number of confirmations into the timeout function for
	// easy telemetry.
	// 基于 confirm 数目来构建 confirm 处理器
	s.timeoutFn = func() {
		fn(int(atomic.LoadInt32(&s.n)))
	}

	// If there aren't any confirmations to be made then take the min
	// time from the start.
	// 若 k 为 0，则将超时时限设置为最小值 min。
	timeout := max
	if k < 1 {
		timeout = min
	}
	s.timer = time.AfterFunc(timeout, s.timeoutFn)

	// Capture the start time right after starting the timer above so
	// we should always err on the side of a little longer timeout if
	// there's any preemption that separates this and the step above.
	s.start = time.Now()
	return s
}

// remainingSuspicionTime takes the state variables of the suspicion timer and
// calculates the remaining time to wait before considering a node dead. The
// return value can be negative, so be prepared to fire the timer immediately in
// that case.
func remainingSuspicionTime(n, k int32, elapsed time.Duration, min, max time.Duration) time.Duration {
	frac := math.Log(float64(n)+1.0) / math.Log(float64(k)+1.0)
	raw := max.Seconds() - frac*(max.Seconds()-min.Seconds())
	timeout := time.Duration(math.Floor(1000.0*raw)) * time.Millisecond
	if timeout < min {
		timeout = min
	}

	// We have to take into account the amount of time that has passed so
	// far, so we get the right overall timeout.
	// 需要减去已经过去的时间。上面计算的时间是一个总超时时间。
	return timeout - elapsed
}

// Confirm registers that a possibly new peer has also determined the given
// node is suspect. This returns true if this was new information, and false
// if it was a duplicate confirmation, or if we've got enough confirmations to
// hit the minimum.
// confirm 操作即表示集群中其它的节点也认为目标节点处于 suspect 状态。
// 因此每当其收到的一个 suspect 消息，会执行一个 confirm 操作。
func (s *suspicion) Confirm(from string) bool {
	// If we've got enough confirmations then stop accepting them.
	// 若收到的 confirm 数已经达到预期的 k 值，则表示我们已经收到足够的 confirm 了，
	//即 已经可以确定目标节点处于 dead 状态了。
	if atomic.LoadInt32(&s.n) >= s.k {
		return false
	}

	// Only allow one confirmation from each possible peer.
	// 需要对其它节点发送的 suspect 消息进行去重，每一个节点只允许触发一次 confirm 操作。
	if _, ok := s.confirmations[from]; ok {
		return false
	}
	s.confirmations[from] = struct{}{}

	// Compute the new timeout given the current number of confirmations and
	// adjust the timer. If the timeout becomes negative *and* we can cleanly
	// stop the timer then we will call the timeout function directly from
	// here.
	// 更新当前的执行的 confirm 次数，根据当前时间戳、执行的 confirm 次数，最小最大次数 以此来更新超时定时器时限。
	// 若发现更新后的剩余时间已经小于0，则直接停止定时器，同时执行对应的超时处理器函数。
	n := atomic.AddInt32(&s.n, 1)
	elapsed := time.Since(s.start)
	remaining := remainingSuspicionTime(n, s.k, elapsed, s.min, s.max)
	if s.timer.Stop() {
		if remaining > 0 {
			s.timer.Reset(remaining)
		} else {
			go s.timeoutFn()
		}
	}
	return true
}
