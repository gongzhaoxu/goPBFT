package pbft

//视图转换
import (
	"encoding/base64"
	"fmt"
	"reflect"

	"github.com/hyperledger/fabric/consensus/util/events"
)

/*
	域转换是保证活性和纠正错误的唯一手段。总结 pbft 模块中发起域转换的时机，可以有以下几种：

	三阶段共识过程中
	1.pre-prepare 消息冲突：收到的 pre-prepare 消息中的域编号和请求序号相同，但请求哈希不同
	2.三阶段共识处理超时，触发 viewChangeTimerEvent 事件

	域转换过程中
	1.发送 view-change 消息后，未能在预定时间内收到多于 2f+1 个其它结点发送的 view-change 消息，触发 viewChangeResendTimerEvent 事件
	2.接收到 view-change 消息后，发现有多于 f 个结点发送的 view-change 消息中的域编号各不相同且都大于自己当前的域编号，因而挑选里面最小的域编号作为自己的域编号，并发送 view-change 消息
	3.接收到 new-view 消息后，但从消息中找不出合规的 checkpoint 集合
	4.收到 new-view 消息后，从消息中计算出的重新进行共识处理的集合为空（pbftCore.assignSequenceNumbers 返回 nil）
	5.收到 new-ivew 消白后，从消息中计算出的重新进行共识处理的集合（调用 pbftCore.assignSequenceNumbers 获得）与 new-view 消息中的 Xset（即「理论篇」中 new-view 消息中的集合 O） 不相同

	其它情况
	1.预定时间内未收到主结点的 pre-prepare 消息（包括空请求）
	2.收到的 pre-prepare 消息中的请求序号大于 pbftCore.viewChangeSeqNo （即应该发起域转换了）
	3.收到 commit 消息时，发现请求处于 committed 状态，且执行请求提交后，发现 commit.SequenceNumber == instance.viewChangeSeqNo （即执行完这一请求，就应该发起域转换了）
*/

//发送视图转换
func (instance *pbftCore) sendViewChange() events.Event {
	// 停止 pbftCore.newViewTimer 计时器
	// 此计时器在超时时会调用 pbftCore.sendViewChange
	instance.stopTimer()

	// 将当前的 new-view 消息从 pbftCore.newViewStore 中删除
	// 此字段记录了自己发送的或接收的、最新的、但还没处理完的 new-view 消息。
	// 在接收到 new-view 消息时，用这个字段判断相同域编号的 new-view 消息是否已经在处理中
	delete(instance.newViewStore, instance.view)
	// 增加 view 域号
	instance.view++

	instance.activeView = false

	//计算两个集合：pset 和 qset。
	//其中 pset 包含了当前结点中最新的 checkpoint 之后的所有 prepared 状态的请求的信息；
	//qset 包含了当前结点中最新的 checkpoint 之后的所有 pre-prepare 消息的信息。
	instance.pset = instance.calcPSet()
	instance.qset = instance.calcQSet()

	//由于我们已经将域编号加 1 了，
	//并且也从 pbftCore.certStore 中计算出了 pset 和 qset，
	//因此这里将 pbftCore.certStore 和 pbftCore.viewChangeStore 中旧的域中的信息删除。
	for idx := range instance.certStore {
		if idx.v < instance.view {
			delete(instance.certStore, idx)
		}
	}
	for idx := range instance.viewChangeStore {
		if idx.v < instance.view {
			delete(instance.viewChangeStore, idx)
		}
	}
	//构造 ViewChange 对象
	vc := &ViewChange{
		View:      instance.view,
		H:         instance.h,
		ReplicaId: instance.id,
	}

	for n, id := range instance.chkpts {
		vc.Cset = append(vc.Cset, &ViewChange_C{
			SequenceNumber: n,
			Id:             id,
		})
	}

	for _, p := range instance.pset {
		if p.SequenceNumber < instance.h {
			logger.Errorf("BUG! Replica %d should not have anything in our pset less than h, found %+v", instance.id, p)
		}
		vc.Pset = append(vc.Pset, p)
	}

	for _, q := range instance.qset {
		if q.SequenceNumber < instance.h {
			logger.Errorf("BUG! Replica %d should not have anything in our qset less than h, found %+v", instance.id, q)
		}
		vc.Qset = append(vc.Qset, q)
	}
	//最后对 ViewChange 对象签名（是的， view-change 消息是有签名的）。
	instance.sign(vc)

	logger.Infof("Replica %d sending view-change, v:%d, h:%d, |C|:%d, |P|:%d, |Q|:%d",
		instance.id, vc.View, vc.H, len(vc.Cset), len(vc.Pset), len(vc.Qset))

	//将刚才构造的 ViewChange 对象广播，
	//然后启动 pbftCore.vcResendTimer 计时器
	//（此计时器超时后会重新发送 view-change 消息，
	//且域编号和刚才发送的 view-change 消息一样）。
	//最后调用 pbftCore.recvViewChange ，
	//即自己率先处理自己广播的 view-change 消息。
	instance.innerBroadcast(&Message{Payload: &Message_ViewChange{ViewChange: vc}})

	instance.vcResendTimer.Reset(instance.vcResendTimeout, viewChangeResendTimerEvent{})

	return instance.recvViewChange(vc)
}

func (instance *pbftCore) correctViewChange(vc *ViewChange) bool {
	for _, p := range append(vc.Pset, vc.Qset...) {
		if !(p.View < vc.View && p.SequenceNumber > vc.H && p.SequenceNumber <= vc.H+instance.L) {
			logger.Debugf("Replica %d invalid p entry in view-change: vc(v:%d h:%d) p(v:%d n:%d)",
				instance.id, vc.View, vc.H, p.View, p.SequenceNumber)
			return false
		}
	}

	for _, c := range vc.Cset {
		// PBFT: the paper says c.n > vc.h
		if !(c.SequenceNumber >= vc.H && c.SequenceNumber <= vc.H+instance.L) {
			logger.Debugf("Replica %d invalid c entry in view-change: vc(v:%d h:%d) c(n:%d)",
				instance.id, vc.View, vc.H, c.SequenceNumber)
			return false
		}
	}

	return true
}

func (instance *pbftCore) calcPSet() map[uint64]*ViewChange_PQ {
	pset := make(map[uint64]*ViewChange_PQ)

	for n, p := range instance.pset {
		pset[n] = p
	}

	// P set：这里已经准备好的请求
	//
	// "<n,d,v> 有准备好的证书，没有请求
	// 在以后的视图中使用相同的编号准备"

	for idx, cert := range instance.certStore {
		if cert.prePrepare == nil {
			continue
		}

		digest := cert.digest
		if !instance.prepared(digest, idx.v, idx.n) {
			continue
		}

		if p, ok := pset[idx.n]; ok && p.View > idx.v {
			continue
		}

		pset[idx.n] = &ViewChange_PQ{
			SequenceNumber: idx.n,
			BatchDigest:    digest,
			View:           idx.v,
		}
	}

	return pset
}

func (instance *pbftCore) calcQSet() map[qidx]*ViewChange_PQ {
	qset := make(map[qidx]*ViewChange_PQ)

	for n, q := range instance.qset {
		qset[n] = q
	}

	// Q set: 这里已经预先准备好的请求（pre-prepare or
	//准备发送）
	//
	// "<n,d,v>: 在这里预先准备的请求，没有
	// 在以后的视图中使用相同的编号进行预准备”

	for idx, cert := range instance.certStore {
		if cert.prePrepare == nil {
			continue
		}

		digest := cert.digest
		if !instance.prePrepared(digest, idx.v, idx.n) {
			continue
		}

		qi := qidx{digest, idx.n}
		if q, ok := qset[qi]; ok && q.View > idx.v {
			continue
		}

		qset[qi] = &ViewChange_PQ{
			SequenceNumber: idx.n,
			BatchDigest:    digest,
			View:           idx.v,
		}
	}

	return qset
}

//处理 view-change 消息
func (instance *pbftCore) recvViewChange(vc *ViewChange) events.Event {
	//对新收到的 view-change 消息的有效性的判断
	logger.Infof("Replica %d received view-change from replica %d, v:%d, h:%d, |C|:%d, |P|:%d, |Q|:%d",
		instance.id, vc.ReplicaId, vc.View, vc.H, len(vc.Cset), len(vc.Pset), len(vc.Qset))
	//pbftCore.verify 用来验证 view-change 消息是否有效；
	if err := instance.verify(vc); err != nil {
		logger.Warningf("Replica %d found incorrect signature in view-change message: %s", instance.id, err)
		return nil
	}
	//判断新的 view-change 中的域编号是否比结点自己的大
	if vc.View < instance.view {
		logger.Warningf("Replica %d found view-change message for old view", instance.id)
		return nil
	}
	//判断 view-change 消息中的一些数据是否正确，
	//包含 pset 和 qset 中的域编号是否比 view-change 中新的域编号小、
	//pset 和 qset 中的请求序号是否位于合理区间内、cset 中的数据是否正确
	if !instance.correctViewChange(vc) {
		logger.Warningf("Replica %d found view-change message incorrect", instance.id)
		return nil
	}

	if _, ok := instance.viewChangeStore[vcidx{vc.View, vc.ReplicaId}]; ok {
		logger.Warningf("Replica %d already has a view change message for view %d from replica %d", instance.id, vc.View, vc.ReplicaId)
		return nil
	}

	instance.viewChangeStore[vcidx{vc.View, vc.ReplicaId}] = vc

	// PBFT TOCS 4.5.2 Liveness: ......
	// 统计总共收到了多少个结点发送了
	// 域编号比结点自己的大的 view-change 消息
	replicas := make(map[uint64]bool)
	minView := uint64(0)
	for idx := range instance.viewChangeStore {
		if idx.v <= instance.view {
			continue
		}

		replicas[idx.id] = true
		if minView == 0 || idx.v < minView {
			minView = idx.v
		}
	}

	//一个结点如果收到了多于 f+1 个其它结点发送的 view-change 消息，
	//且这些消息的域编号都比自己当前的大，那么就立即从这些消息中选一个最小的域编号
	//发送 view-change 消息
	if len(replicas) >= instance.f+1 {
		logger.Infof("Replica %d received f+1 view-change messages, triggering view-change to view %d",
			instance.id, minView)
		instance.view = minView - 1
		return instance.sendViewChange()
	}
	//统计有多少 view-chnage 消息的域编号与结点自己的相同；
	//如果此数量达到要求（pbftCore.allCorrectReplicasQuorum()，
	//即 2f+1），则返回 viewChangeQuorumEvent 对象。
	quorum := 0
	for idx := range instance.viewChangeStore {
		if idx.v == instance.view {
			quorum++
		}
	}
	logger.Debugf("Replica %d now has %d view change requests for view %d", instance.id, quorum, instance.view)

	if !instance.activeView && vc.View == instance.view && quorum >= instance.allCorrectReplicasQuorum() {
		instance.vcResendTimer.Stop()
		instance.startTimer(instance.lastNewViewTimeout, "new view change")
		instance.lastNewViewTimeout = 2 * instance.lastNewViewTimeout
		return viewChangeQuorumEvent{}
	}

	return nil
}

func (instance *pbftCore) sendNewView() events.Event {

	if _, ok := instance.newViewStore[instance.view]; ok {
		logger.Debugf("Replica %d already has new view in store for view %d, skipping", instance.id, instance.view)
		return nil
	}

	vset := instance.getViewChanges()

	cp, ok, _ := instance.selectInitialCheckpoint(vset)
	if !ok {
		logger.Infof("Replica %d could not find consistent checkpoint: %+v", instance.id, instance.viewChangeStore)
		return nil
	}

	msgList := instance.assignSequenceNumbers(vset, cp.SequenceNumber)
	if msgList == nil {
		logger.Infof("Replica %d could not assign sequence numbers for new view", instance.id)
		return nil
	}

	nv := &NewView{
		View:      instance.view,
		Vset:      vset,
		Xset:      msgList,
		ReplicaId: instance.id,
	}

	logger.Infof("Replica %d is new primary, sending new-view, v:%d, X:%+v",
		instance.id, nv.View, nv.Xset)

	instance.innerBroadcast(&Message{Payload: &Message_NewView{NewView: nv}})
	instance.newViewStore[instance.view] = nv
	return instance.processNewView()
}

func (instance *pbftCore) recvNewView(nv *NewView) events.Event {
	logger.Infof("Replica %d received new-view %d",
		instance.id, nv.View)

	if !(nv.View > 0 && nv.View >= instance.view && instance.primary(nv.View) == nv.ReplicaId && instance.newViewStore[nv.View] == nil) {
		logger.Infof("Replica %d rejecting invalid new-view from %d, v:%d",
			instance.id, nv.ReplicaId, nv.View)
		return nil
	}

	for _, vc := range nv.Vset {
		if err := instance.verify(vc); err != nil {
			logger.Warningf("Replica %d found incorrect view-change signature in new-view message: %s", instance.id, err)
			return nil
		}
	}

	instance.newViewStore[nv.View] = nv
	return instance.processNewView()
}

func (instance *pbftCore) processNewView() events.Event {
	var newReqBatchMissing bool
	nv, ok := instance.newViewStore[instance.view]
	if !ok {
		logger.Debugf("Replica %d ignoring processNewView as it could not find view %d in its newViewStore", instance.id, instance.view)
		return nil
	}

	if instance.activeView {
		logger.Infof("Replica %d ignoring new-view from %d, v:%d: we are active in view %d",
			instance.id, nv.ReplicaId, nv.View, instance.view)
		return nil
	}

	cp, ok, replicas := instance.selectInitialCheckpoint(nv.Vset)
	if !ok {
		logger.Warningf("Replica %d could not determine initial checkpoint: %+v",
			instance.id, instance.viewChangeStore)
		return instance.sendViewChange()
	}

	speculativeLastExec := instance.lastExec
	if instance.currentExec != nil {
		speculativeLastExec = *instance.currentExec
	}

	// If we have not reached the sequence number, check to see if we can reach it without state transfer
	// In general, executions are better than state transfer
	if speculativeLastExec < cp.SequenceNumber {
		canExecuteToTarget := true
	outer:
		for seqNo := speculativeLastExec + 1; seqNo <= cp.SequenceNumber; seqNo++ {
			found := false
			for idx, cert := range instance.certStore {
				if idx.n != seqNo {
					continue
				}

				quorum := 0
				for _, p := range cert.commit {
					// Was this committed in the previous view
					if p.View == idx.v && p.SequenceNumber == seqNo {
						quorum++
					}
				}

				if quorum < instance.intersectionQuorum() {
					logger.Debugf("Replica %d missing quorum of commit certificate for seqNo=%d, only has %d of %d", instance.id, quorum, instance.intersectionQuorum())
					continue
				}

				found = true
				break
			}

			if !found {
				canExecuteToTarget = false
				logger.Debugf("Replica %d missing commit certificate for seqNo=%d", instance.id, seqNo)
				break outer
			}

		}

		if canExecuteToTarget {
			logger.Debugf("Replica %d needs to process a new view, but can execute to the checkpoint seqNo %d, delaying processing of new view", instance.id, cp.SequenceNumber)
			return nil
		}

		logger.Infof("Replica %d cannot execute to the view change checkpoint with seqNo %d", instance.id, cp.SequenceNumber)
	}

	msgList := instance.assignSequenceNumbers(nv.Vset, cp.SequenceNumber)
	if msgList == nil {
		logger.Warningf("Replica %d could not assign sequence numbers: %+v",
			instance.id, instance.viewChangeStore)
		return instance.sendViewChange()
	}

	if !(len(msgList) == 0 && len(nv.Xset) == 0) && !reflect.DeepEqual(msgList, nv.Xset) {
		logger.Warningf("Replica %d failed to verify new-view Xset: computed %+v, received %+v",
			instance.id, msgList, nv.Xset)
		return instance.sendViewChange()
	}

	if instance.h < cp.SequenceNumber {
		instance.moveWatermarks(cp.SequenceNumber)
	}

	if speculativeLastExec < cp.SequenceNumber {
		logger.Warningf("Replica %d missing base checkpoint %d (%s), our most recent execution %d", instance.id, cp.SequenceNumber, cp.Id, speculativeLastExec)

		snapshotID, err := base64.StdEncoding.DecodeString(cp.Id)
		if nil != err {
			err = fmt.Errorf("Replica %d received a view change whose hash could not be decoded (%s)", instance.id, cp.Id)
			logger.Error(err.Error())
			return nil
		}

		target := &stateUpdateTarget{
			checkpointMessage: checkpointMessage{
				seqNo: cp.SequenceNumber,
				id:    snapshotID,
			},
			replicas: replicas,
		}

		instance.updateHighStateTarget(target)
		instance.stateTransfer(target)
	}

	for n, d := range nv.Xset {
		// PBFT: why should we use "h ≥ min{n | ∃d : (<n,d> ∈ X)}"?
		// "h ≥ min{n | ∃d : (<n,d> ∈ X)} ∧ ∀<n,d> ∈ X : (n ≤ h ∨ ∃m ∈ in : (D(m) = d))"
		if n <= instance.h {
			continue
		} else {
			if d == "" {
				// NULL request; skip
				continue
			}

			if _, ok := instance.reqBatchStore[d]; !ok {
				logger.Warningf("Replica %d missing assigned, non-checkpointed request batch %s",
					instance.id, d)
				if _, ok := instance.missingReqBatches[d]; !ok {
					logger.Warningf("Replica %v requesting to fetch batch %s",
						instance.id, d)
					newReqBatchMissing = true
					instance.missingReqBatches[d] = true
				}
			}
		}
	}

	if len(instance.missingReqBatches) == 0 {
		return instance.processNewView2(nv)
	} else if newReqBatchMissing {
		instance.fetchRequestBatches()
	}

	return nil
}

func (instance *pbftCore) processNewView2(nv *NewView) events.Event {
	logger.Infof("Replica %d accepting new-view to view %d", instance.id, instance.view)

	instance.stopTimer()
	instance.nullRequestTimer.Stop()

	instance.activeView = true
	delete(instance.newViewStore, instance.view-1)

	instance.seqNo = instance.h
	for n, d := range nv.Xset {
		if n <= instance.h {
			continue
		}

		reqBatch, ok := instance.reqBatchStore[d]
		if !ok && d != "" {
			logger.Criticalf("Replica %d is missing request batch for seqNo=%d with digest '%s' for assigned prepare after fetching, this indicates a serious bug", instance.id, n, d)
		}
		preprep := &PrePrepare{
			View:           instance.view,
			SequenceNumber: n,
			BatchDigest:    d,
			RequestBatch:   reqBatch,
			ReplicaId:      instance.id,
		}
		cert := instance.getCert(instance.view, n)
		cert.prePrepare = preprep
		cert.digest = d
		if n > instance.seqNo {
			instance.seqNo = n
		}
		instance.persistQSet()
	}

	instance.updateViewChangeSeqNo()

	if instance.primary(instance.view) != instance.id {
		for n, d := range nv.Xset {
			prep := &Prepare{
				View:           instance.view,
				SequenceNumber: n,
				BatchDigest:    d,
				ReplicaId:      instance.id,
			}
			if n > instance.h {
				cert := instance.getCert(instance.view, n)
				cert.sentPrepare = true
				instance.recvPrepare(prep)
			}
			instance.innerBroadcast(&Message{Payload: &Message_Prepare{Prepare: prep}})
		}
	} else {
		logger.Debugf("Replica %d is now primary, attempting to resubmit requests", instance.id)
		instance.resubmitRequestBatches()
	}

	instance.startTimerIfOutstandingRequests()

	logger.Debugf("Replica %d done cleaning view change artifacts, calling into consumer", instance.id)

	return viewChangedEvent{}
}

func (instance *pbftCore) getViewChanges() (vset []*ViewChange) {
	for _, vc := range instance.viewChangeStore {
		vset = append(vset, vc)
	}

	return
}

func (instance *pbftCore) selectInitialCheckpoint(vset []*ViewChange) (checkpoint ViewChange_C, ok bool, replicas []uint64) {
	checkpoints := make(map[ViewChange_C][]*ViewChange)
	for _, vc := range vset {
		for _, c := range vc.Cset { // TODO, verify that we strip duplicate checkpoints from this set
			checkpoints[*c] = append(checkpoints[*c], vc)
			logger.Debugf("Replica %d appending checkpoint from replica %d with seqNo=%d, h=%d, and checkpoint digest %s", instance.id, vc.ReplicaId, vc.H, c.SequenceNumber, c.Id)
		}
	}

	if len(checkpoints) == 0 {
		logger.Debugf("Replica %d has no checkpoints to select from: %d %s",
			instance.id, len(instance.viewChangeStore), checkpoints)
		return
	}

	for idx, vcList := range checkpoints {
		// need weak certificate for the checkpoint
		if len(vcList) <= instance.f { // type casting necessary to match types
			logger.Debugf("Replica %d has no weak certificate for n:%d, vcList was %d long",
				instance.id, idx.SequenceNumber, len(vcList))
			continue
		}

		quorum := 0
		// Note, this is the whole vset (S) in the paper, not just this checkpoint set (S') (vcList)
		// We need 2f+1 low watermarks from S below this seqNo from all replicas
		// We need f+1 matching checkpoints at this seqNo (S')
		for _, vc := range vset {
			if vc.H <= idx.SequenceNumber {
				quorum++
			}
		}

		if quorum < instance.intersectionQuorum() {
			logger.Debugf("Replica %d has no quorum for n:%d", instance.id, idx.SequenceNumber)
			continue
		}

		replicas = make([]uint64, len(vcList))
		for i, vc := range vcList {
			replicas[i] = vc.ReplicaId
		}

		if checkpoint.SequenceNumber <= idx.SequenceNumber {
			checkpoint = idx
			ok = true
		}
	}

	return
}

func (instance *pbftCore) assignSequenceNumbers(vset []*ViewChange, h uint64) (msgList map[uint64]string) {
	msgList = make(map[uint64]string)

	maxN := h + 1

	// "for all n such that h < n <= h + L"
nLoop:
	for n := h + 1; n <= h+instance.L; n++ {
		// "∃m ∈ S..."
		for _, m := range vset {
			// "...with <n,d,v> ∈ m.P"
			for _, em := range m.Pset {
				quorum := 0
				// "A1. ∃2f+1 messages m' ∈ S"
			mpLoop:
				for _, mp := range vset {
					if mp.H >= n {
						continue
					}
					// "∀<n,d',v'> ∈ m'.P"
					for _, emp := range mp.Pset {
						if n == emp.SequenceNumber && !(emp.View < em.View || (emp.View == em.View && emp.BatchDigest == em.BatchDigest)) {
							continue mpLoop
						}
					}
					quorum++
				}

				if quorum < instance.intersectionQuorum() {
					continue
				}

				quorum = 0
				// "A2. ∃f+1 messages m' ∈ S"
				for _, mp := range vset {
					// "∃<n,d',v'> ∈ m'.Q"
					for _, emp := range mp.Qset {
						if n == emp.SequenceNumber && emp.View >= em.View && emp.BatchDigest == em.BatchDigest {
							quorum++
						}
					}
				}

				if quorum < instance.f+1 {
					continue
				}

				// "then select the request with digest d for number n"
				msgList[n] = em.BatchDigest
				maxN = n

				continue nLoop
			}
		}

		quorum := 0
		// "else if ∃2f+1 messages m ∈ S"
	nullLoop:
		for _, m := range vset {
			// "m.P has no entry"
			for _, em := range m.Pset {
				if em.SequenceNumber == n {
					continue nullLoop
				}
			}
			quorum++
		}

		if quorum >= instance.intersectionQuorum() {
			// "then select the null request for number n"
			msgList[n] = ""

			continue nLoop
		}

		logger.Warningf("Replica %d could not assign value to contents of seqNo %d, found only %d missing P entries", instance.id, n, quorum)
		return nil
	}

	// prune top null requests
	for n, msg := range msgList {
		if n > maxN && msg == "" {
			delete(msgList, n)
		}
	}

	return
}
