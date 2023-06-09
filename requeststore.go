package pbft

//实现了 requestStore 对象。这个对象实现了内存中的请求的管理。
import "container/list"

type requestStore struct {
	outstandingRequests *orderedRequests
	pendingRequests     *orderedRequests
}

// newRequestStore 创建一个新的 requestStore。
func newRequestStore() *requestStore {
	rs := &requestStore{
		outstandingRequests: &orderedRequests{},
		pendingRequests:     &orderedRequests{},
	}
	// 初始化数据结构
	rs.outstandingRequests.empty()
	rs.pendingRequests.empty()

	return rs
}

// storeOutstanding 添加一个请求到未完成的请求列表
func (rs *requestStore) storeOutstanding(request *Request) {
	rs.outstandingRequests.add(request)
}

// storePending 将请求添加到待处理请求列表
func (rs *requestStore) storePending(request *Request) {
	rs.pendingRequests.add(request)
}

//storePending 将一部分请求添加到待处理请求列表中
func (rs *requestStore) storePendings(requests []*Request) {
	rs.pendingRequests.adds(requests)
}

// remove 从 outstanding 和 pending 列表中删除请求，
//它分别返回是否在每个列表中找到
func (rs *requestStore) remove(request *Request) (outstanding, pending bool) {
	outstanding = rs.outstandingRequests.remove(request)
	pending = rs.pendingRequests.remove(request)
	return
}

// // getNextNonPending 返回下一个 n 个未完成但不是挂起的请求
func (rs *requestStore) hasNonPending() bool {
	return rs.outstandingRequests.Len() > rs.pendingRequests.Len()
}

// getNextNonPending 返回下一个 n 个未完成但不是挂起的请求
func (rs *requestStore) getNextNonPending(n int) (result []*Request) {
	for oreqc := rs.outstandingRequests.order.Front(); oreqc != nil; oreqc = oreqc.Next() {
		oreq := oreqc.Value.(requestContainer)
		if rs.pendingRequests.has(oreq.key) {
			continue
		}
		result = append(result, oreq.req)
		if len(result) == n {
			break
		}
	}

	return result
}

type requestContainer struct {
	key string
	req *Request
}

type orderedRequests struct {
	order    list.List
	presence map[string]*list.Element
}

func (a *orderedRequests) Len() int {
	return a.order.Len()
}

func (a *orderedRequests) wrapRequest(req *Request) requestContainer {
	return requestContainer{
		key: hash(req),
		req: req,
	}
}

func (a *orderedRequests) has(key string) bool {
	_, ok := a.presence[key]
	return ok
}

func (a *orderedRequests) add(request *Request) {
	rc := a.wrapRequest(request)
	if !a.has(rc.key) {
		e := a.order.PushBack(rc)
		a.presence[rc.key] = e
	}
}

func (a *orderedRequests) adds(requests []*Request) {
	for _, req := range requests {
		a.add(req)
	}
}

func (a *orderedRequests) remove(request *Request) bool {
	rc := a.wrapRequest(request)
	e, ok := a.presence[rc.key]
	if !ok {
		return false
	}
	a.order.Remove(e)
	delete(a.presence, rc.key)
	return true
}

func (a *orderedRequests) removes(requests []*Request) bool {
	allSuccess := true
	for _, req := range requests {
		if !a.remove(req) {
			allSuccess = false
		}
	}

	return allSuccess
}

func (a *orderedRequests) empty() {
	a.order.Init()
	a.presence = make(map[string]*list.Element)
}
