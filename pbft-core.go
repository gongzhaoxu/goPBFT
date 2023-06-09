package pbft

/*
实现了 pbftCore 对象及其方法。这是 PBFT 算法的核心实现，
还包含 pbft-persist.go、sign.go、
*/
import (
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/hyperledger/fabric/consensus"
	"github.com/hyperledger/fabric/consensus/util/events"
	_ "github.com/hyperledger/fabric/core" // Needed for logging format init
	"github.com/op/go-logging"

	"github.com/spf13/viper"
)

// =============================================================================
// 初始化
// =============================================================================

var logger *logging.Logger // 日志

func init() {
	logger = logging.MustGetLogger("consensus/pbft")
}

const (
	//意外计时器，防止系统有莫名其妙的事件，设置一个大的计时器
	UnreasonableTimeout = 100 * time.Hour
)

// =============================================================================
// 自定义接口和结构体定义
// =============================================================================

// 事件类型

// 驱动工作
type workEvent func()

// 视图转换计时器
type viewChangeTimerEvent struct{}

// 执行完成时发送
type execDoneEvent struct{}

//当收到要发送给 pbft 的共识消息时发送
type pbftMessageEvent pbftMessage

// 视图转换计时器
type viewChangedEvent struct{}

// 视图转换重新发送
type viewChangeResendTimerEvent struct{}

// 转发请求时发送
type returnRequestBatchEvent *RequestBatch

// "keep-alive"空请求
type nullRequestEvent struct{}

// 内部栈
type innerStack interface {
	broadcast(msgPayload []byte)
	unicast(msgPayload []byte, receiverID uint64) (err error)
	execute(seqNo uint64, reqBatch *RequestBatch) // This is invoked on a separate thread
	getState() []byte
	getLastSeqNo() (uint64, error)
	skipTo(seqNo uint64, snapshotID []byte, peers []uint64)

	sign(msg []byte) ([]byte, error)
	verify(senderID uint64, signature []byte, message []byte) error

	invalidateState()
	validateState()

	consensus.StatePersistor
}

//传入 PBFT 绑定消息
type pbftMessage struct {
	sender uint64
	msg    *Message
}

//视图转换
type checkpointMessage struct {
	seqNo uint64
	id    []byte
}

type stateUpdateTarget struct {
	checkpointMessage
	replicas []uint64
}

//最核心的结构体定义
type pbftCore struct {
	internalLock sync.Mutex
	executing    bool // 程序执行标志

	consumer innerStack

	// PBFT 结构体数据
	activeView    bool              // 视图转换正在发生
	byzantine     bool              // 拜占庭标志
	f             int               // 最大容忍的拜占庭个数
	N             int               // 网络中最大节点总数
	h             uint64            // 低水位线
	id            uint64            // 节点Id
	K             uint64            // 检查点跨度
	logMultiplier uint64            // 日志大小 : k*logMultiplier
	L             uint64            // 日志大小
	lastExec      uint64            // 上次执行的请求
	replicaCount  int               // 节点个数
	seqNo         uint64            // 序列号
	view          uint64            // 当前视图
	chkpts        map[uint64]string // 检查点状态
	pset          map[uint64]*ViewChange_PQ
	qset          map[qidx]*ViewChange_PQ

	skipInProgress    bool               //当我们检测到落后情况时设置，直到我们选择一个新的起点
	stateTransferring bool               // 执行状态转移时设置
	highStateTarget   *stateUpdateTarget // 设置为我们观察到的最高弱检查点证书
	hChkpts           map[uint64]uint64  // 最高水位

	currentExec           *uint64                  // 当前执行的请求
	timerActive           bool                     // 计时器是否运行中
	vcResendTimer         events.Timer             // 计时器触发重新发送视图更改
	newViewTimer          events.Timer             // 超时触发视图更改
	requestTimeout        time.Duration            // 请求超时
	vcResendTimeout       time.Duration            // 重新发送视图更改前超时
	newViewTimeout        time.Duration            // 新视图的进度超时
	newViewTimerReason    string                   // 是什么触发了计时器
	lastNewViewTimeout    time.Duration            // 我们在此视图更改期间使用的上次超时
	broadcastTimeout      time.Duration            // 广播进度超时
	outstandingReqBatches map[string]*RequestBatch // 跟踪我们是否正在等待请求批处理执行

	nullRequestTimer   events.Timer  // 超时触发空请求
	nullRequestTimeout time.Duration // 此超时的持续时间
	viewChangePeriod   uint64        // 自动视图更改之间的时间
	viewChangeSeqNo    uint64        // 下一个 seqNo 执行视图更改

	missingReqBatches map[string]bool // 对于所有分配的、非检查点的请求批次，我们在视图更改期间可能会丢失

	// 数据存储和回复
	reqBatchStore   map[string]*RequestBatch // 跟踪请求批次
	certStore       map[msgID]*msgCert       // 跟踪请求的仲裁证书
	checkpointStore map[Checkpoint]bool      // 跟踪设置的检查点
	viewChangeStore map[vcidx]*ViewChange    // 跟踪视图更改消息
	newViewStore    map[uint64]*NewView      // 跟踪我们收到或发送的最后一个新视图
}

/*
	字段存储了所有最近处理的请求的信息（无论成功的或进行中的）
	存储的正是 PBFT 共识三阶段的所有消息
*/
type msgCert struct {
	digest      string
	prePrepare  *PrePrepare
	sentPrepare bool
	prepare     []*Prepare
	sentCommit  bool
	commit      []*Commit
}

//==================================
//需要的集合结构体定义
type qidx struct {
	d string
	n uint64
}

//certStore索引
type msgID struct {
	v uint64
	n uint64
}

type vcidx struct {
	v  uint64
	id uint64
}

// =============================================================================
// 构造newPbftCore
// =============================================================================

func newPbftCore(id uint64, config *viper.Viper, consumer innerStack, etf events.TimerFactory) *pbftCore {
	var err error
	instance := &pbftCore{}
	instance.id = id
	instance.consumer = consumer

	instance.newViewTimer = etf.CreateTimer()
	instance.vcResendTimer = etf.CreateTimer()
	instance.nullRequestTimer = etf.CreateTimer()

	instance.N = config.GetInt("general.N")
	instance.f = config.GetInt("general.f")
	if instance.f*3+1 > instance.N {
		panic(fmt.Sprintf("need at least %d enough replicas to tolerate %d byzantine faults, but only %d replicas configured", instance.f*3+1, instance.f, instance.N))
	}

	instance.K = uint64(config.GetInt("general.K"))

	instance.logMultiplier = uint64(config.GetInt("general.logmultiplier"))
	if instance.logMultiplier < 2 {
		panic("Log multiplier must be greater than or equal to 2")
	}
	instance.L = instance.logMultiplier * instance.K // 日志大小
	instance.viewChangePeriod = uint64(config.GetInt("general.viewchangeperiod"))

	instance.byzantine = config.GetBool("general.byzantine")

	instance.requestTimeout, err = time.ParseDuration(config.GetString("general.timeout.request"))
	if err != nil {
		panic(fmt.Errorf("Cannot parse request timeout: %s", err))
	}
	instance.vcResendTimeout, err = time.ParseDuration(config.GetString("general.timeout.resendviewchange"))
	if err != nil {
		panic(fmt.Errorf("Cannot parse request timeout: %s", err))
	}
	instance.newViewTimeout, err = time.ParseDuration(config.GetString("general.timeout.viewchange"))
	if err != nil {
		panic(fmt.Errorf("Cannot parse new view timeout: %s", err))
	}
	instance.nullRequestTimeout, err = time.ParseDuration(config.GetString("general.timeout.nullrequest"))
	if err != nil {
		instance.nullRequestTimeout = 0
	}
	instance.broadcastTimeout, err = time.ParseDuration(config.GetString("general.timeout.broadcast"))
	if err != nil {
		panic(fmt.Errorf("Cannot parse new broadcast timeout: %s", err))
	}

	instance.activeView = true
	instance.replicaCount = instance.N

	logger.Infof("PBFT type = %T", instance.consumer)
	logger.Infof("PBFT Max number of validating peers (N) = %v", instance.N)
	logger.Infof("PBFT Max number of failing peers (f) = %v", instance.f)
	logger.Infof("PBFT byzantine flag = %v", instance.byzantine)
	logger.Infof("PBFT request timeout = %v", instance.requestTimeout)
	logger.Infof("PBFT view change timeout = %v", instance.newViewTimeout)
	logger.Infof("PBFT Checkpoint period (K) = %v", instance.K)
	logger.Infof("PBFT broadcast timeout = %v", instance.broadcastTimeout)
	logger.Infof("PBFT Log multiplier = %v", instance.logMultiplier)
	logger.Infof("PBFT log size (L) = %v", instance.L)
	if instance.nullRequestTimeout > 0 {
		logger.Infof("PBFT null requests timeout = %v", instance.nullRequestTimeout)
	} else {
		logger.Infof("PBFT null requests disabled")
	}
	if instance.viewChangePeriod > 0 {
		logger.Infof("PBFT view change period = %v", instance.viewChangePeriod)
	} else {
		logger.Infof("PBFT automatic view change disabled")
	}

	// 初始化日志
	instance.certStore = make(map[msgID]*msgCert)
	instance.reqBatchStore = make(map[string]*RequestBatch)
	instance.checkpointStore = make(map[Checkpoint]bool)
	instance.chkpts = make(map[uint64]string)
	instance.viewChangeStore = make(map[vcidx]*ViewChange)
	instance.pset = make(map[uint64]*ViewChange_PQ)
	instance.qset = make(map[qidx]*ViewChange_PQ)
	instance.newViewStore = make(map[uint64]*NewView)

	//初始状态转移
	instance.hChkpts = make(map[uint64]uint64)

	instance.chkpts[0] = "XXX GENESIS"

	instance.lastNewViewTimeout = instance.newViewTimeout
	instance.outstandingReqBatches = make(map[string]*RequestBatch)
	instance.missingReqBatches = make(map[string]bool)

	instance.restoreState()

	instance.viewChangeSeqNo = ^uint64(0)
	instance.updateViewChangeSeqNo()

	return instance
}

// 释放资源
func (instance *pbftCore) close() {
	instance.newViewTimer.Halt()
	instance.nullRequestTimer.Halt()
}

// 允许视图更改协议在计时器到期时启动
func (instance *pbftCore) ProcessEvent(e events.Event) events.Event {
	var err error
	logger.Debugf("Replica %d processing event", instance.id)
	switch et := e.(type) {
	case viewChangeTimerEvent:
		logger.Infof("Replica %d view change timer expired, sending view change: %s", instance.id, instance.newViewTimerReason)
		instance.timerActive = false
		instance.sendViewChange()
	case *pbftMessage:
		return pbftMessageEvent(*et)
	case pbftMessageEvent:
		msg := et
		logger.Debugf("Replica %d received incoming message from %v", instance.id, msg.sender)
		next, err := instance.recvMsg(msg.msg, msg.sender)
		if err != nil {
			break
		}
		return next
	/*
		对 *RequestBatch 分支的处理特别简单，
		就是直接调用 pbftCore.recvRequestBatch 进行处
	*/
	case *RequestBatch:
		err = instance.recvRequestBatch(et)
	case *PrePrepare:
		err = instance.recvPrePrepare(et)
	case *Prepare:
		err = instance.recvPrepare(et)
	case *Commit:
		err = instance.recvCommit(et)
	case *Checkpoint:
		return instance.recvCheckpoint(et)
	case *ViewChange:
		return instance.recvViewChange(et)
	case *NewView:
		return instance.recvNewView(et)
	case *FetchRequestBatch:
		err = instance.recvFetchRequestBatch(et)
	case returnRequestBatchEvent:
		return instance.recvReturnRequestBatch(et)
	case stateUpdatedEvent:
		update := et.chkpt
		instance.stateTransferring = false
		if et.target == nil || update.seqNo < instance.h {
			if et.target == nil {
				logger.Warningf("Replica %d attempted state transfer target was not reachable (%v)", instance.id, et.chkpt)
			} else {
				logger.Warningf("Replica %d recovered to seqNo %d but our low watermark has moved to %d", instance.id, update.seqNo, instance.h)
			}
			if instance.highStateTarget == nil {
				logger.Debugf("Replica %d has no state targets, cannot resume state transfer yet", instance.id)
			} else if update.seqNo < instance.highStateTarget.seqNo {
				logger.Debugf("Replica %d has state target for %d, transferring", instance.id, instance.highStateTarget.seqNo)
				instance.retryStateTransfer(nil)
			} else {
				logger.Debugf("Replica %d has no state target above %d, highest is %d", instance.id, update.seqNo, instance.highStateTarget.seqNo)
			}
			return nil
		}
		logger.Infof("Replica %d application caught up via state transfer, lastExec now %d", instance.id, update.seqNo)
		instance.lastExec = update.seqNo
		instance.moveWatermarks(instance.lastExec) // 水位移动处理将其移动到检查点边界
		instance.skipInProgress = false
		instance.consumer.validateState()
		instance.Checkpoint(update.seqNo, update.id)
		instance.executeOutstanding()
	case execDoneEvent:
		instance.execDoneSync()
		if instance.skipInProgress {
			instance.retryStateTransfer(nil)
		}
		// 我们有时会延迟新视图处理
		return instance.processNewView()
	case nullRequestEvent:
		instance.nullRequestHandler()
	case workEvent:
		et() // 用于允许调用者窃取主线程的使用，将被删除
	case viewChangeQuorumEvent:
		logger.Debugf("Replica %d received view change quorum, processing new view", instance.id)
		if instance.primary(instance.view) == instance.id {
			return instance.sendNewView()
		}
		return instance.processNewView()
	case viewChangedEvent:
		//空操作，如果需要由插件处理
	case viewChangeResendTimerEvent:
		if instance.activeView {
			logger.Warningf("Replica %d had its view change resend timer expire but it's in an active view, this is benign but may indicate a bug", instance.id)
			return nil
		}
		logger.Debugf("Replica %d view change resend timer expired before view change quorum was reached, resending", instance.id)
		instance.view-- // 发送视图更改会增加此
		return instance.sendViewChange()
	default:
		logger.Warningf("Replica %d received an unknown message type %T", instance.id, et)
	}

	if err != nil {
		logger.Warning(err.Error())
	}

	return nil
}

// 给定摘要/视图/序列，certLog 中是否有条目？
// 如果是，则返回它。 如果没有，创建它。
func (instance *pbftCore) getCert(v uint64, n uint64) (cert *msgCert) {
	idx := msgID{v, n}
	cert, ok := instance.certStore[idx]
	if ok {
		return
	}

	cert = &msgCert{}
	instance.certStore[idx] = cert
	return
}

// =============================================================================
// preprepare/prepare/commit 状态检查
// =============================================================================

// intersectionQuorum 返回必须的副本数
// 同意保证至少有一个正确的副本被共享
func (instance *pbftCore) intersectionQuorum() int {
	return (instance.N + instance.f + 2) / 2
}

// allCorrectReplicasQuorum returns the number of correct replicas (N-f)
func (instance *pbftCore) allCorrectReplicasQuorum() int {
	return (instance.N - instance.f)
}

//判断当前请求是否处于 prepared 状态。
func (instance *pbftCore) prePrepared(digest string, v uint64, n uint64) bool {
	_, mInLog := instance.reqBatchStore[digest]

	if digest != "" && !mInLog {
		return false
	}

	if q, ok := instance.qset[qidx{digest, n}]; ok && q.View == v {
		return true
	}

	cert := instance.certStore[msgID{v, n}]
	if cert != nil {
		p := cert.prePrepare
		if p != nil && p.View == v && p.SequenceNumber == n && p.BatchDigest == digest {
			return true
		}
	}
	logger.Debugf("Replica %d does not have view=%d/seqNo=%d pre-prepared",
		instance.id, v, n)
	return false
}

//判断当前请求是否处于 prepared 状态。如果是则构造 commit 消息并广播。
func (instance *pbftCore) prepared(digest string, v uint64, n uint64) bool {
	if !instance.prePrepared(digest, v, n) {
		return false
	}

	if p, ok := instance.pset[n]; ok && p.View == v && p.BatchDigest == digest {
		return true
	}

	quorum := 0
	cert := instance.certStore[msgID{v, n}]
	/*
		首先这个请求得是 pre-prepared 状态，也就是接受了请求的 pre-prepare 消息；
		然后如果 pbftCore.pset 字段存在这个请求，也算是处于 prepared 状态了；
		最后就是从 msgCert.prepare 找出符合要求的 prepare 消息的数量，
		如果这个数量大于等于 pbftCore.intersectionQuorum() - 1，则认为处于 prepared 状态了。
	*/
	if cert == nil {
		return false
	}

	for _, p := range cert.prepare {
		if p.View == v && p.SequenceNumber == n && p.BatchDigest == digest {
			quorum++
		}
	}

	logger.Debugf("Replica %d prepare count for view=%d/seqNo=%d: %d",
		instance.id, v, n, quorum)

	return quorum >= instance.intersectionQuorum()-1
}

//判断当前请求是否处于 committed 状态。
func (instance *pbftCore) committed(digest string, v uint64, n uint64) bool {
	//首先请求必须已经处于 prepared 状态
	if !instance.prepared(digest, v, n) {
		return false
	}

	quorum := 0
	cert := instance.certStore[msgID{v, n}]
	if cert == nil {
		return false
	}
	//统计当前请求的有效的 commit 消息的数量，
	//如果大于或等于 pbftCore.intersectionQuorum 方法的返回值
	//（实际为 2f+1），则说明已处于 committed 状态。
	for _, p := range cert.commit {
		if p.View == v && p.SequenceNumber == n {
			quorum++
		}
	}

	logger.Debugf("Replica %d commit count for view=%d/seqNo=%d: %d",
		instance.id, v, n, quorum)

	return quorum >= instance.intersectionQuorum()
}

// =============================================================================
// 接收消息
// =============================================================================

//逻辑非常简单，就是判断并转换参数 msg 为真正的类型，并返回
func (instance *pbftCore) recvMsg(msg *Message, senderID uint64) (interface{}, error) {
	if reqBatch := msg.GetRequestBatch(); reqBatch != nil {
		return reqBatch, nil
	} else if preprep := msg.GetPrePrepare(); preprep != nil {
		//判断发送者的 ID senderID 是否与 PrePrepare.ReplicaId 一致
		if senderID != preprep.ReplicaId {
			return nil, fmt.Errorf("Sender ID included in pre-prepare message (%v) doesn't match ID corresponding to the receiving stream (%v)", preprep.ReplicaId, senderID)
		}
		return preprep, nil
	} else if prep := msg.GetPrepare(); prep != nil {
		// 对发送者和 Prepare 中保存的 ID 进行比较。
		// 前面我们说过这是在无需签名的情况下确保数据真实性的重要手段
		if senderID != prep.ReplicaId {
			return nil, fmt.Errorf("Sender ID included in prepare message (%v) doesn't match ID corresponding to the receiving stream (%v)", prep.ReplicaId, senderID)
		}
		return prep, nil
	} else if commit := msg.GetCommit(); commit != nil {
		if senderID != commit.ReplicaId {
			return nil, fmt.Errorf("Sender ID included in commit message (%v) doesn't match ID corresponding to the receiving stream (%v)", commit.ReplicaId, senderID)
		}
		return commit, nil
	} else if chkpt := msg.GetCheckpoint(); chkpt != nil {
		if senderID != chkpt.ReplicaId {
			return nil, fmt.Errorf("Sender ID included in checkpoint message (%v) doesn't match ID corresponding to the receiving stream (%v)", chkpt.ReplicaId, senderID)
		}
		return chkpt, nil
	} else if vc := msg.GetViewChange(); vc != nil {
		if senderID != vc.ReplicaId {
			return nil, fmt.Errorf("Sender ID included in view-change message (%v) doesn't match ID corresponding to the receiving stream (%v)", vc.ReplicaId, senderID)
		}
		return vc, nil
	} else if nv := msg.GetNewView(); nv != nil {
		if senderID != nv.ReplicaId {
			return nil, fmt.Errorf("Sender ID included in new-view message (%v) doesn't match ID corresponding to the receiving stream (%v)", nv.ReplicaId, senderID)
		}
		return nv, nil
	} else if fr := msg.GetFetchRequestBatch(); fr != nil {
		if senderID != fr.ReplicaId {
			return nil, fmt.Errorf("Sender ID included in fetch-request-batch message (%v) doesn't match ID corresponding to the receiving stream (%v)", fr.ReplicaId, senderID)
		}
		return fr, nil
	} else if reqBatch := msg.GetReturnRequestBatch(); reqBatch != nil {
		// 发送者 ID 和副本 ID 可以不同； 我们正在发送原始请求消息
		return returnRequestBatchEvent(reqBatch), nil
	}
	return nil, fmt.Errorf("Invalid message: %v", msg)
}

/*
接收并处理批请求
*/
func (instance *pbftCore) recvRequestBatch(reqBatch *RequestBatch) error {
	digest := hash(reqBatch)
	logger.Debugf("Replica %d received request batch %s", instance.id, digest)

	// 保存 *RequestBatch 对象
	instance.reqBatchStore[digest] = reqBatch
	instance.outstandingReqBatches[digest] = reqBatch
	instance.persistRequestBatch(digest)

	// 收到请求后启动计时器。如果超时代表请求处理失败，要发起 View Change。
	if instance.activeView {
		instance.softStartTimer(
			instance.requestTimeout,
			fmt.Sprintf("new request batch %s", digest))
	}
	// 如果自己是主结点，主要马上发送 pre-prepare 消息
	if instance.primary(instance.view) == instance.id && instance.activeView {
		instance.nullRequestTimer.Stop()
		instance.sendPrePrepare(reqBatch, digest)
	} else {
		logger.Debugf("Replica %d is backup, not sending pre-prepare for request batch %s", instance.id, digest)
	}
	return nil
}

//发送 pre-prepare 消息
func (instance *pbftCore) sendPrePrepare(reqBatch *RequestBatch, digest string) {
	logger.Debugf("Replica %d is primary, issuing pre-prepare for request batch %s", instance.id, digest)

	n := instance.seqNo + 1
	// 检查是否存在 RequestBatch 的哈希相同、但 SequenceNumber 不同的 pre-prepare 消息
	for _, cert := range instance.certStore { //
		if p := cert.prePrepare; p != nil {
			if p.View == instance.view && p.SequenceNumber != n && p.BatchDigest == digest && digest != "" {
				logger.Infof("Other pre-prepare found with same digest but different seqNo: %d instead of %d", p.SequenceNumber, n)
				return
			}
		}
	}
	/*
		这里主要判断新的序号和域编号是否合规
	*/
	if !instance.inWV(instance.view, n) || n > instance.h+instance.L/2 {
		logger.Warningf("Primary %d not sending pre-prepare for batch %s - out of sequence numbers", instance.id, digest)
		return
	}

	if n > instance.viewChangeSeqNo {
		logger.Info("Primary %d about to switch to next primary, not sending pre-prepare with seqno=%d", instance.id, n)
		return
	}

	logger.Debugf("Primary %d broadcasting pre-prepare for view=%d/seqNo=%d and digest %s", instance.id, instance.view, n, digest)

	//如果刚才的检查都成功，那么继续进行处理，发送 pre-prepare 消息：
	instance.seqNo = n
	preprep := &PrePrepare{
		View:           instance.view,
		SequenceNumber: n,
		BatchDigest:    digest,
		RequestBatch:   reqBatch,
		ReplicaId:      instance.id,
	}
	cert := instance.getCert(instance.view, n)
	cert.prePrepare = preprep
	cert.digest = digest
	instance.persistQSet()
	instance.innerBroadcast(&Message{Payload: &Message_PrePrepare{PrePrepare: preprep}})
	instance.maybeSendCommit(digest, instance.view, n)
}

func (instance *pbftCore) resubmitRequestBatches() {
	if instance.primary(instance.view) != instance.id {
		return
	}

	var submissionOrder []*RequestBatch

outer:
	for d, reqBatch := range instance.outstandingReqBatches {
		for _, cert := range instance.certStore {
			if cert.digest == d {
				logger.Debugf("Replica %d already has certificate for request batch %s - not going to resubmit", instance.id, d)
				continue outer
			}
		}
		logger.Debugf("Replica %d has detected request batch %s must be resubmitted", instance.id, d)
		submissionOrder = append(submissionOrder, reqBatch)
	}

	if len(submissionOrder) == 0 {
		return
	}

	for _, reqBatch := range submissionOrder {
		// This is a request batch that has not been pre-prepared yet
		// Trigger request batch processing again
		instance.recvRequestBatch(reqBatch)
	}
}

//处理pre-prepare消息
func (instance *pbftCore) recvPrePrepare(preprep *PrePrepare) error {
	logger.Debugf("Replica %d received pre-prepare from replica %d for view=%d/seqNo=%d",
		instance.id, preprep.ReplicaId, preprep.View, preprep.SequenceNumber)

	if !instance.activeView {
		logger.Debugf("Replica %d ignoring pre-prepare as we are in a view change", instance.id)
		return nil
	}
	// 自已当前的主结点 ID 与 pre-prepare 中发送者 ID 不同，则不处理
	if instance.primary(instance.view) != preprep.ReplicaId {
		logger.Warningf("Pre-prepare from other than primary: got %d, should be %d", preprep.ReplicaId, instance.primary(instance.view))
		return nil
	}
	// 如果 pre-prepare 中的域编号与自己当前的不一致，
	// 或者 pre-prepare 中的序号不在 [h, H] 区间内，
	// 则不处理
	if !instance.inWV(preprep.View, preprep.SequenceNumber) {
		if preprep.SequenceNumber != instance.h && !instance.skipInProgress {
			logger.Warningf("Replica %d pre-prepare view different, or sequence number outside watermarks: preprep.View %d, expected.View %d, seqNo %d, low-mark %d", instance.id, preprep.View, instance.primary(instance.view), preprep.SequenceNumber, instance.h)
		} else {
			// This is perfectly normal
			logger.Debugf("Replica %d pre-prepare view different, or sequence number outside watermarks: preprep.View %d, expected.View %d, seqNo %d, low-mark %d", instance.id, preprep.View, instance.primary(instance.view), preprep.SequenceNumber, instance.h)
		}

		return nil
	}
	// 如果马上要进行域转换了，也不处理当前消息
	if preprep.SequenceNumber > instance.viewChangeSeqNo {
		logger.Info("Replica %d received pre-prepare for %d, which should be from the next primary", instance.id, preprep.SequenceNumber)
		instance.sendViewChange()
		return nil
	}
	// 根据域编号和序号获取相应的共识信息，
	// 判断存在请求哈希一致、但序号和域编号不一致的情况。如果存在则发起域转换。
	cert := instance.getCert(preprep.View, preprep.SequenceNumber)
	if cert.digest != "" && cert.digest != preprep.BatchDigest {
		logger.Warningf("Pre-prepare found for same view/seqNo but different digest: received %s, stored %s", preprep.BatchDigest, cert.digest)
		instance.sendViewChange()
		return nil
	}

	cert.prePrepare = preprep
	cert.digest = preprep.BatchDigest

	// 如果出于某种原因我们没有从之前的广播中收到请求，则存储请求批次
	/*
		将 pre-prepare 消息存储到 msgCert 结构（pbftCore.certStore 字段中的 value）中。
		如果自己没有保存请求数据，则现在保存一份。
	*/
	if _, ok := instance.reqBatchStore[preprep.BatchDigest]; !ok && preprep.BatchDigest != "" {
		digest := hash(preprep.GetRequestBatch())
		if digest != preprep.BatchDigest {
			logger.Warningf("Pre-prepare and request digest do not match: request %s, digest %s", digest, preprep.BatchDigest)
			return nil
		}
		instance.reqBatchStore[digest] = preprep.GetRequestBatch()
		logger.Debugf("Replica %d storing request batch %s in outstanding request batch store", instance.id, digest)
		instance.outstandingReqBatches[digest] = preprep.GetRequestBatch()
		instance.persistRequestBatch(digest)
	}

	instance.softStartTimer(instance.requestTimeout, fmt.Sprintf("new pre-prepare for request batch %s", preprep.BatchDigest))
	instance.nullRequestTimer.Stop()
	//========================================================
	// if 判断：
	//   1. 自己不是主结点
	//   2. 我们已经接受了当前请求的 pre-prepare 消息
	//   3. 还未发送过 prepare 消息
	if instance.primary(instance.view) != instance.id && instance.prePrepared(preprep.BatchDigest, preprep.View, preprep.SequenceNumber) && !cert.sentPrepare {
		logger.Debugf("Backup %d broadcasting prepare for view=%d/seqNo=%d", instance.id, preprep.View, preprep.SequenceNumber)
		prep := &Prepare{
			View:           preprep.View,
			SequenceNumber: preprep.SequenceNumber,
			BatchDigest:    preprep.BatchDigest,
			ReplicaId:      instance.id,
		}
		cert.sentPrepare = true
		instance.persistQSet()
		instance.recvPrepare(prep)
		return instance.innerBroadcast(&Message{Payload: &Message_Prepare{Prepare: prep}})
	}

	return nil
}

//处理 prepare 消息
func (instance *pbftCore) recvPrepare(prep *Prepare) error {
	logger.Debugf("Replica %d received prepare from replica %d for view=%d/seqNo=%d",
		instance.id, prep.ReplicaId, prep.View, prep.SequenceNumber)
	// 如果是主结点发送的 prpare 消息，则忽略它
	if instance.primary(prep.View) == prep.ReplicaId {
		logger.Warningf("Replica %d received prepare from primary, ignoring", instance.id)
		return nil
	}
	//对 Prepare 对象中域编号和请求序号的检查与处理 PrePrepare 类似
	if !instance.inWV(prep.View, prep.SequenceNumber) {
		if prep.SequenceNumber != instance.h && !instance.skipInProgress {
			logger.Warningf("Replica %d ignoring prepare for view=%d/seqNo=%d: not in-wv, in view %d, low water mark %d", instance.id, prep.View, prep.SequenceNumber, instance.view, instance.h)
		} else {
			// This is perfectly normal
			logger.Debugf("Replica %d ignoring prepare for view=%d/seqNo=%d: not in-wv, in view %d, low water mark %d", instance.id, prep.View, prep.SequenceNumber, instance.view, instance.h)
		}
		return nil
	}

	cert := instance.getCert(prep.View, prep.SequenceNumber)
	/*
		从 pbftCore.certStore 中获取请求的共识相关信息；
		然后判断是否重复。
		最后将 Prepare 对象记录在 msgCert.prepare 中。
		最后调用 pbftCore.maybeSendCommit，看是否可以已经处于 prepared 状态，
		如果是则发送 commit 消息。
	*/
	for _, prevPrep := range cert.prepare {
		if prevPrep.ReplicaId == prep.ReplicaId {
			logger.Warningf("Ignoring duplicate prepare from %d", prep.ReplicaId)
			return nil
		}
	}
	cert.prepare = append(cert.prepare, prep)
	instance.persistPSet()

	return instance.maybeSendCommit(prep.BatchDigest, prep.View, prep.SequenceNumber)
}

//
func (instance *pbftCore) maybeSendCommit(digest string, v uint64, n uint64) error {
	cert := instance.getCert(v, n)
	if instance.prepared(digest, v, n) && !cert.sentCommit {
		logger.Debugf("Replica %d broadcasting commit for view=%d/seqNo=%d",
			instance.id, v, n)
		commit := &Commit{
			View:           v,
			SequenceNumber: n,
			BatchDigest:    digest,
			ReplicaId:      instance.id,
		}
		cert.sentCommit = true
		instance.recvCommit(commit)
		return instance.innerBroadcast(&Message{&Message_Commit{commit}})
	}
	return nil
}

//处理 commit 消息
func (instance *pbftCore) recvCommit(commit *Commit) error {
	logger.Debugf("Replica %d received commit from replica %d for view=%d/seqNo=%d",
		instance.id, commit.ReplicaId, commit.View, commit.SequenceNumber)
	//对 Commit 对象中域编号和请求序号的检查与处理 PrePrepare 类似，这里就不再啰嗦了：
	if !instance.inWV(commit.View, commit.SequenceNumber) {
		if commit.SequenceNumber != instance.h && !instance.skipInProgress {
			logger.Warningf("Replica %d ignoring commit for view=%d/seqNo=%d: not in-wv, in view %d, low water mark %d", instance.id, commit.View, commit.SequenceNumber, instance.view, instance.h)
		} else {
			logger.Debugf("Replica %d ignoring commit for view=%d/seqNo=%d: not in-wv, in view %d, low water mark %d", instance.id, commit.View, commit.SequenceNumber, instance.view, instance.h)
		}
		return nil
	}

	cert := instance.getCert(commit.View, commit.SequenceNumber)
	// 去掉重复
	for _, prevCommit := range cert.commit {
		if prevCommit.ReplicaId == commit.ReplicaId {
			logger.Warningf("Ignoring duplicate commit from %d", commit.ReplicaId)
			return nil
		}
	}
	cert.commit = append(cert.commit, commit)

	if instance.committed(commit.BatchDigest, commit.View, commit.SequenceNumber) {
		instance.stopTimer()
		instance.lastNewViewTimeout = instance.newViewTimeout
		// 已经 commited，因此将从请从「未解决」的 outstandingReqBatches 中删掉
		delete(instance.outstandingReqBatches, commit.BatchDigest)

		instance.executeOutstanding()
		// 到了该进行域转换序号了
		if commit.SequenceNumber == instance.viewChangeSeqNo {
			logger.Infof("Replica %d cycling view for seqNo=%d", instance.id, commit.SequenceNumber)
			instance.sendViewChange()
		}
	}

	return nil
}

func (instance *pbftCore) updateHighStateTarget(target *stateUpdateTarget) {
	if instance.highStateTarget != nil && instance.highStateTarget.seqNo >= target.seqNo {
		logger.Debugf("Replica %d not updating state target to seqNo %d, has target for seqNo %d", instance.id, target.seqNo, instance.highStateTarget.seqNo)
		return
	}

	instance.highStateTarget = target
}

func (instance *pbftCore) stateTransfer(optional *stateUpdateTarget) {
	if !instance.skipInProgress {
		logger.Debugf("Replica %d is out of sync, pending state transfer", instance.id)
		instance.skipInProgress = true
		instance.consumer.invalidateState()
	}

	instance.retryStateTransfer(optional)
}

func (instance *pbftCore) retryStateTransfer(optional *stateUpdateTarget) {
	if instance.currentExec != nil {
		logger.Debugf("Replica %d is currently mid-execution, it must wait for the execution to complete before performing state transfer", instance.id)
		return
	}

	if instance.stateTransferring {
		logger.Debugf("Replica %d is currently mid state transfer, it must wait for this state transfer to complete before initiating a new one", instance.id)
		return
	}

	target := optional
	if target == nil {
		if instance.highStateTarget == nil {
			logger.Debugf("Replica %d has no targets to attempt state transfer to, delaying", instance.id)
			return
		}
		target = instance.highStateTarget
	}

	instance.stateTransferring = true

	logger.Debugf("Replica %d is initiating state transfer to seqNo %d", instance.id, target.seqNo)
	instance.consumer.skipTo(target.seqNo, target.id, target.replicas)

}

func (instance *pbftCore) executeOutstanding() {
	//判断当前是否有请求在被执行
	//pbftCore.currentExec 为当前正在执行的请求序号的变量指针，
	//它会在 pbftCore.executeOne 中被设置，
	//一旦被设置，就代表就请求正在被执行。
	if instance.currentExec != nil {
		logger.Debugf("Replica %d not attempting to executeOutstanding because it is currently executing %d", instance.id, *instance.currentExec)
		return
	}
	logger.Debugf("Replica %d attempting to executeOutstanding", instance.id)

	for idx := range instance.certStore {
		if instance.executeOne(idx) {
			break
		}
	}

	logger.Debugf("Replica %d certstore %+v", instance.id, instance.certStore)

	instance.startTimerIfOutstandingRequests()
}

func (instance *pbftCore) executeOne(idx msgID) bool {
	cert := instance.certStore[idx]
	//当前要执行的请求的序号是否是最近一次执行的请求的序号加 1
	if idx.n != instance.lastExec+1 || cert == nil || cert.prePrepare == nil {
		return false
	}
	//判断 pbftCore.skipInProgress 的值。
	//这个值在进行状态同步的时候会设置为 true，用来暂停请求的执行。
	if instance.skipInProgress {
		logger.Debugf("Replica %d currently picking a starting point to resume, will not execute", instance.id)
		return false
	}

	// 如果请求处于 committed 状态，则进行下面的处理：

	digest := cert.digest
	reqBatch := instance.reqBatchStore[digest]

	if !instance.committed(digest, idx.v, idx.n) {
		return false
	}
	/*
	   这里又分两种情况，如果是一个空请求，就什么也不做，
	   直接调用 pbftCore.execDoneSync
	   处理请求已执行完的情况；如果不是空请求，
	   则调用 pbftCore.consumer 中的 execute 方法执行请求。
	   其中 pbftCore.consumer 实际上指向的是 obcBatch 对象，
	   因此这里实际上调用的是 obcBatch.execute 方法。
	*/
	currentExec := idx.n
	instance.currentExec = &currentExec

	// 空请求
	if digest == "" {
		logger.Infof("Replica %d executing/committing null request for view=%d/seqNo=%d",
			instance.id, idx.v, idx.n)
		instance.execDoneSync()
	} else {
		logger.Infof("Replica %d executing/committing request batch for view=%d/seqNo=%d and digest %s",
			instance.id, idx.v, idx.n, digest)
		instance.consumer.execute(idx.n, reqBatch)
	}
	return true
}

func (instance *pbftCore) Checkpoint(seqNo uint64, id []byte) {
	if seqNo%instance.K != 0 {
		logger.Errorf("Attempted to checkpoint a sequence number (%d) which is not a multiple of the checkpoint interval (%d)", seqNo, instance.K)
		return
	}

	idAsString := base64.StdEncoding.EncodeToString(id)

	logger.Debugf("Replica %d preparing checkpoint for view=%d/seqNo=%d and b64 id of %s",
		instance.id, instance.view, seqNo, idAsString)

	chkpt := &Checkpoint{
		SequenceNumber: seqNo,
		ReplicaId:      instance.id,
		Id:             idAsString,
	}
	instance.chkpts[seqNo] = idAsString

	instance.persistCheckpoint(seqNo, id)
	instance.recvCheckpoint(chkpt)
	instance.innerBroadcast(&Message{Payload: &Message_Checkpoint{Checkpoint: chkpt}})
}

func (instance *pbftCore) execDoneSync() {
	if instance.currentExec != nil {
		logger.Infof("Replica %d finished execution %d, trying next", instance.id, *instance.currentExec)
		instance.lastExec = *instance.currentExec
		if instance.lastExec%instance.K == 0 {
			instance.Checkpoint(instance.lastExec, instance.consumer.getState())
		}

	} else {
		// XXX This masks a bug, this should not be called when currentExec is nil
		logger.Warningf("Replica %d had execDoneSync called, flagging ourselves as out of date", instance.id)
		instance.skipInProgress = true
	}
	instance.currentExec = nil

	instance.executeOutstanding()
}

func (instance *pbftCore) softStartTimer(timeout time.Duration, reason string) {
	logger.Debugf("Replica %d soft starting new view timer for %s: %s", instance.id, timeout, reason)
	instance.newViewTimerReason = reason
	instance.timerActive = true
	instance.newViewTimer.SoftReset(timeout, viewChangeTimerEvent{})
}

func (instance *pbftCore) startTimer(timeout time.Duration, reason string) {
	logger.Debugf("Replica %d starting new view timer for %s: %s", instance.id, timeout, reason)
	instance.timerActive = true
	instance.newViewTimer.Reset(timeout, viewChangeTimerEvent{})
}

func (instance *pbftCore) stopTimer() {
	logger.Debugf("Replica %d stopping a running new view timer", instance.id)
	instance.timerActive = false
	instance.newViewTimer.Stop()
}
