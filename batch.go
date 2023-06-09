package pbft

// 实现了 obcBatch 对象其及方法。这个对象在稍高的层次上
//包装了 PBFT，它包含了 PBFT 执行的主要流程，也是共识接口
//（consensus.Consenter）的直接实现。但 PBFT 算法却并不在
//这个对象中直接实现的，而是在 pbftCore 中。

//ProcessEvent函数中是一个大的 switch/case，用来处理各种各样的消息。
import (
	"fmt"
	"time"

	"github.com/hyperledger/fabric/consensus"
	"github.com/hyperledger/fabric/consensus/util/events"
	pb "github.com/hyperledger/fabric/protos"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/spf13/viper"
)

type obcBatch struct {
	obcGeneric
	externalEventReceiver
	pbft        *pbftCore
	broadcaster *broadcaster

	batchSize        int //每一批存多少个
	batchStore       []*Request
	batchTimer       events.Timer
	batchTimerActive bool
	batchTimeout     time.Duration

	incomingChan chan *batchMessage // 排队消息以供主线程处理

	reqStore *requestStore // 保留未完成和待处理的请求，试图转换时用

	deduplicator *deduplicator

	persistForward
}

type batchMessage struct {
	msg    *pb.Message
	sender *pb.PeerID
}

// 事件类型

//batchMessageEvent 在收到共识消息时发送给 pbft
type batchMessageEvent batchMessage

// batchTimerEvent 在批处理计时器到期时发送
type batchTimerEvent struct{}

//构造obcbatch对象
func newObcBatch(id uint64, config *viper.Viper, stack consensus.Stack) *obcBatch {
	var err error

	op := &obcBatch{
		obcGeneric: obcGeneric{stack: stack},
	}

	op.persistForward.persistor = stack

	logger.Debugf("Replica %d obtaining startup information", id)

	op.manager.SetReceiver(op)
	etf := events.NewTimerFactoryImpl(op.manager)
	op.pbft = newPbftCore(id, config, op, etf)
	op.manager.Start()
	blockchainInfoBlob := stack.GetBlockchainInfoBlob()
	op.externalEventReceiver.manager = op.manager
	op.broadcaster = newBroadcaster(id, op.pbft.N, op.pbft.f, op.pbft.broadcastTimeout, stack)
	op.manager.Queue() <- workEvent(func() {
		op.pbft.stateTransfer(&stateUpdateTarget{
			checkpointMessage: checkpointMessage{
				seqNo: op.pbft.lastExec,
				id:    blockchainInfoBlob,
			},
		})
	})

	op.batchSize = config.GetInt("general.batchsize")
	op.batchStore = nil
	op.batchTimeout, err = time.ParseDuration(config.GetString("general.timeout.batch"))
	if err != nil {
		panic(fmt.Errorf("Cannot parse batch timeout: %s", err))
	}
	logger.Infof("PBFT Batch size = %d", op.batchSize)
	logger.Infof("PBFT Batch timeout = %v", op.batchTimeout)

	if op.batchTimeout >= op.pbft.requestTimeout {
		op.pbft.requestTimeout = 3 * op.batchTimeout / 2
		logger.Warningf("Configured request timeout must be greater than batch timeout, setting to %v", op.pbft.requestTimeout)
	}

	if op.pbft.requestTimeout >= op.pbft.nullRequestTimeout && op.pbft.nullRequestTimeout != 0 {
		op.pbft.nullRequestTimeout = 3 * op.pbft.requestTimeout / 2
		logger.Warningf("Configured null request timeout must be greater than request timeout, setting to %v", op.pbft.nullRequestTimeout)
	}

	op.incomingChan = make(chan *batchMessage)

	op.batchTimer = etf.CreateTimer()

	op.reqStore = newRequestStore()

	op.deduplicator = newDeduplicator()

	op.idleChan = make(chan struct{})

	return op
}

// 关闭时释放相应资源
func (op *obcBatch) Close() {
	op.batchTimer.Halt()
	op.pbft.close()
}

//请求给主结点
func (op *obcBatch) submitToLeader(req *Request) events.Event {
	// 首先请求广播到全网，以防现在处于错误的视图
	op.broadcastMsg(&BatchMessage{Payload: &BatchMessage_Request{Request: req}})
	op.logAddTxFromRequest(req)
	op.reqStore.storeOutstanding(req)
	op.startTimerIfOutstandingRequests()
	//接下来才会去判断自己是不是主结点
	if op.pbft.primary(op.pbft.view) == op.pbft.id && op.pbft.activeView {
		return op.leaderProcReq(req)
	}
	return nil
}

func (op *obcBatch) broadcastMsg(msg *BatchMessage) {
	msgPayload, _ := proto.Marshal(msg)
	ocMsg := &pb.Message{
		Type:    pb.Message_CONSENSUS,
		Payload: msgPayload,
	}
	op.broadcaster.Broadcast(ocMsg)
}

// =============================================================================
// 消息批处理相关函数
// =============================================================================

func (op *obcBatch) leaderProcReq(req *Request) events.Event {
	// 检查摘要
	digest := hash(req)
	logger.Debugf("Batch primary %d queueing new request %s", op.pbft.id, digest)
	//将 Request 对象存储到 obcBatch.batchStore 字段
	op.batchStore = append(op.batchStore, req)
	//存储
	op.reqStore.storePending(req)
	//检查各类计时器
	if !op.batchTimerActive {
		op.startBatchTimer()
	}

	if len(op.batchStore) >= op.batchSize {
		return op.sendBatch()
	}

	return nil
}

//简单地使用 obcBatch.batchStore 中
//所有 Request 对象构造一个 *RequestBatch 对象，然后将其返回
func (op *obcBatch) sendBatch() events.Event {
	op.stopBatchTimer()
	if len(op.batchStore) == 0 {
		logger.Error("Told to send an empty batch store for ordering, ignoring")
		return nil
	}

	reqBatch := &RequestBatch{Batch: op.batchStore}
	op.batchStore = nil
	logger.Infof("Creating batch with %d requests", len(reqBatch.Batch))
	return reqBatch
}

//将一条 Transaction转变成 Request 请求对象，
//然后调用 obcBatch.submitToLeader 提交这个请求给主结点：
func (op *obcBatch) txToReq(tx []byte) *Request {
	now := time.Now()
	req := &Request{
		Timestamp: &timestamp.Timestamp{
			Seconds: now.Unix(),
			Nanos:   int32(now.UnixNano() % 1000000000),
		},
		Payload:   tx,
		ReplicaId: op.pbft.id,
	}
	return req
}

//============================================================
//消息处理
//============================================================

func (op *obcBatch) processMessage(ocMsg *pb.Message, senderHandle *pb.PeerID) events.Event {
	//消息类型判断
	if ocMsg.Type == pb.Message_CHAIN_TRANSACTION {
		req := op.txToReq(ocMsg.Payload)
		return op.submitToLeader(req)
	}

	if ocMsg.Type != pb.Message_CONSENSUS {
		logger.Errorf("Unexpected message type: %s", ocMsg.Type)
		return nil
	}
	/*
		首先将请求消息反序列化为一个 BatchMessage 对象，
		这个对象只是一个「壳」，它可能是四种类型的对象之一
	*/
	batchMsg := &BatchMessage{}
	err := proto.Unmarshal(ocMsg.Payload, batchMsg)
	if err != nil {
		logger.Errorf("Error unmarshaling message: %s", err)
		return nil
	}
	/*
		如果 BatchMessage.GetRequest 成功，说明刚才的 BatchMessage
		真正代表的是一个 Request 请求。那么接下来代码首先利用 deduplicator 对象
		判断这个 Request 请求是否是重复的。如果是一个新收到的请求，且自己就是主结点，
		那么就会调用 obcBatch.leaderProcReq 处理这个请求。
	*/
	if req := batchMsg.GetRequest(); req != nil {
		if !op.deduplicator.IsNew(req) {
			logger.Warningf("Replica %d ignoring request as it is too old", op.pbft.id)
			return nil
		}

		op.logAddTxFromRequest(req)
		op.reqStore.storeOutstanding(req)
		if (op.pbft.primary(op.pbft.view) == op.pbft.id) && op.pbft.activeView {
			return op.leaderProcReq(req)
		}
		op.startTimerIfOutstandingRequests()
		return nil
	} else if pbftMsg := batchMsg.GetPbftMessage(); pbftMsg != nil {
		/*
			RequestBatch 请求是包含在 BatchMessage_PbftMessage 消息中
		*/
		senderID, err := getValidatorID(senderHandle) //发送者
		if err != nil {
			panic("Cannot map sender's PeerID to a valid replica ID")
		}
		/*
			将接收到的消息转变成了一个 pbftMessageEvent 对象并返回了。
			然后消息循环会再次使用 pbftMessageEvent 作为参数调用 obcBatch.ProcessEvent
		*/
		msg := &Message{}
		err = proto.Unmarshal(pbftMsg, msg)
		if err != nil {
			logger.Errorf("Error unpacking payload from message: %s", err)
			return nil
		}
		return pbftMessageEvent{
			msg:    msg,
			sender: senderID,
		}
	}

	logger.Errorf("Unknown request: %+v", batchMsg)

	return nil
}

func (op *obcBatch) resubmitOutstandingReqs() events.Event {
	op.startTimerIfOutstandingRequests()

	// If we are the primary, and know of outstanding requests, submit them for inclusion in the next batch until
	// we run out of requests, or a new batch message is triggered (this path will re-enter after execution)
	// Do not enter while an execution is in progress to prevent duplicating a request
	if op.pbft.primary(op.pbft.view) == op.pbft.id && op.pbft.activeView && op.pbft.currentExec == nil {
		needed := op.batchSize - len(op.batchStore)

		for op.reqStore.hasNonPending() {
			outstanding := op.reqStore.getNextNonPending(needed)

			// If we have enough outstanding requests, this will trigger a batch
			for _, nreq := range outstanding {
				if msg := op.leaderProcReq(nreq); msg != nil {
					op.manager.Inject(msg)
				}
			}
		}
	}
	return nil
}

// 允许主节点在计时器到期时发送批处理
// obcBatch.ProcessEvent 中是一个大的 switch/case，用来处理各种各样的消息。
func (op *obcBatch) ProcessEvent(event events.Event) events.Event {
	logger.Debugf("Replica %d batch main thread looping", op.pbft.id)
	switch et := event.(type) {
	case batchMessageEvent:
		ocMsg := et
		return op.processMessage(ocMsg.msg, ocMsg.sender)
	case executedEvent:
		op.stack.Commit(nil, et.tag.([]byte))
	case committedEvent:
		logger.Debugf("Replica %d received committedEvent", op.pbft.id)
		return execDoneEvent{}
	case execDoneEvent:
		if res := op.pbft.ProcessEvent(event); res != nil {
			// 这可能会触发视图转换，如果是那我们将在新视图上重新提交
			return res
		}
		return op.resubmitOutstandingReqs()
	case batchTimerEvent:
		logger.Infof("Replica %d batch timer expired", op.pbft.id)
		if op.pbft.activeView && (len(op.batchStore) > 0) {
			return op.sendBatch()
		}
	case *Commit:
		res := op.pbft.ProcessEvent(event)
		op.startTimerIfOutstandingRequests()
		return res
	case viewChangedEvent:
		op.batchStore = nil
		// Outstanding reqs 对 batch 没有意义，因为一个 batch 中的所有请求都可能被处理
		// 因此，在视图更改时，将其清除
		op.pbft.outstandingReqBatches = make(map[string]*RequestBatch)

		logger.Debugf("Replica %d batch thread recognizing new view", op.pbft.id)
		if op.batchTimerActive {
			op.stopBatchTimer()
		}

		if op.pbft.skipInProgress {
			// 如果我们是新的主节点，但我们正在进行视图转换，我们不能相信自己不会复制东西
			op.reqStore.outstandingRequests.empty()
		}

		op.reqStore.pendingRequests.empty()
		for i := op.pbft.h + 1; i <= op.pbft.h+op.pbft.L; i++ {
			if i <= op.pbft.lastExec {
				continue
			}

			cert, ok := op.pbft.certStore[msgID{v: op.pbft.view, n: i}]
			if !ok || cert.prePrepare == nil {
				continue
			}

			if cert.prePrepare.BatchDigest == "" {
				//空请求
				continue
			}

			if cert.prePrepare.RequestBatch == nil {
				logger.Warningf("Replica %d found a non-null prePrepare with no request batch, ignoring")
				continue
			}

			op.reqStore.storePendings(cert.prePrepare.RequestBatch.GetBatch())
		}

		return op.resubmitOutstandingReqs()
	case stateUpdatedEvent:
		// 当状态更新时，清除任何未完成的请求，它们可能在我们离开时已经被处理
		op.reqStore = newRequestStore()
		return op.pbft.ProcessEvent(event)
	default:
		return op.pbft.ProcessEvent(event)
	}

	return nil
}

func (op *obcBatch) startBatchTimer() {
	op.batchTimer.Reset(op.batchTimeout, batchTimerEvent{})
	logger.Debugf("Replica %d started the batch timer", op.pbft.id)
	op.batchTimerActive = true
}

func (op *obcBatch) stopBatchTimer() {
	op.batchTimer.Stop()
	logger.Debugf("Replica %d stopped the batch timer", op.pbft.id)
	op.batchTimerActive = false
}

func (op *obcBatch) startTimerIfOutstandingRequests() {
	if op.pbft.skipInProgress || op.pbft.currentExec != nil || !op.pbft.activeView {
		// Do not start view change timer if some background event is in progress
		logger.Debugf("Replica %d not starting timer because skip in progress or current exec or in view change", op.pbft.id)
		return
	}

	if !op.reqStore.hasNonPending() {
		// Only start a timer if we are aware of outstanding requests
		logger.Debugf("Replica %d not starting timer because all outstanding requests are pending", op.pbft.id)
		return
	}
	op.pbft.softStartTimer(op.pbft.requestTimeout, "Batch outstanding requests")
}
