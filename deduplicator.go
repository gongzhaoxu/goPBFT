package pbft

//这个文件中的代码实现了 deduplicator 对象其及方法。
//这个对象通过记录其它结点的请求时间戳和执行消息的时间戳，
//可以对重复或无效的消息进行处理。
import (
	"time"
)

/*
维护两个时间戳。
一个时间戳跟踪从副本接收到的最新请求，
另一个超时跟踪最近执行的请求。
*/
type deduplicator struct {
	reqTimestamps  map[uint64]time.Time
	execTimestamps map[uint64]time.Time
}

// newDeduplicator 创建一个新的重复数据删除器。
func newDeduplicator() *deduplicator {
	d := &deduplicator{}
	d.reqTimestamps = make(map[uint64]time.Time)
	d.execTimestamps = make(map[uint64]time.Time)
	return d
}

// 请求更新收到的请求时间戳以进行提交
// 副本。 如果请求早于之前收到的任何请求或
// 已执行的请求，Request() 将返回 false，表示已过时
// 要求。
func (d *deduplicator) Request(req *Request) bool {
	reqTime := time.Unix(req.Timestamp.Seconds, int64(req.Timestamp.Nanos))
	if !reqTime.After(d.reqTimestamps[req.ReplicaId]) ||
		!reqTime.After(d.execTimestamps[req.ReplicaId]) {
		return false
	}
	d.reqTimestamps[req.ReplicaId] = reqTime
	return true
}

// 执行更新提交的执行请求时间戳
// 副本。 如果请求早于任何先前执行的请求
// 来自同一个副本的请求，Execute() 将返回 false，
// 指示过时的请求。
func (d *deduplicator) Execute(req *Request) bool {
	reqTime := time.Unix(req.Timestamp.Seconds, int64(req.Timestamp.Nanos))
	if !reqTime.After(d.execTimestamps[req.ReplicaId]) {
		return false
	}
	d.execTimestamps[req.ReplicaId] = reqTime
	return true
}

// IsNew 如果这个 Request 比之前的任何一个都新则返回 true
// 提交副本的已执行请求。
func (d *deduplicator) IsNew(req *Request) bool {
	reqTime := time.Unix(req.Timestamp.Seconds, int64(req.Timestamp.Nanos))
	return reqTime.After(d.execTimestamps[req.ReplicaId])
}
