//Fabric 中的共识算法使用 protobuf 协议传递消息，
//这个文件就是对 PBFT 中用到的消息的定义。
//由这个文件、使用 protoc-gen-go 程序，
//就可以生成 go 源码文件 messages.pb.go。

/*
 * o: 交易
 * t: 时间戳
 * c: 客户端
 * v: 视图
 * n: 去列好
 * D(m): 请求摘要
 * i: 节点ID
 */

package pbft

import (
	fmt "fmt"

	proto "github.com/golang/protobuf/proto"

	math "math"

	google_protobuf "github.com/golang/protobuf/ptypes/timestamp"
)

var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

const _ = proto.ProtoPackageIsVersion2

//============================================================
//                    各类消息结构体定义
//===========================================================

//处理过程中 Message_CHAIN_TRANSACTION 消息会转变成 Request 请求，
//而多个 Request 请求会集合在一起转变成 RequestBatch
//最终由 pbftCore 对 RequestBatch 进行处理。
type Request struct {
	//时间戳
	Timestamp *google_protobuf.Timestamp `protobuf:"bytes,1,opt,name=timestamp" json:"timestamp,omitempty"`
	//请求操作
	Payload []byte `protobuf:"bytes,2,opt,name=payload,proto3" json:"payload,omitempty"`
	//发起请求的节点
	ReplicaId uint64 `protobuf:"varint,3,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
	//签名
	Signature []byte `protobuf:"bytes,4,opt,name=signature,proto3" json:"signature,omitempty"`
}

//按批处理
type RequestBatch struct {
	//消息集合，存到数组里之后处理
	Batch []*Request `protobuf:"bytes,1,rep,name=batch" json:"batch,omitempty"`
}

type FetchRequestBatch struct {
	//批消息摘要
	BatchDigest string `protobuf:"bytes,1,opt,name=batch_digest,json=batchDigest" json:"batch_digest,omitempty"`
	//节点ID
	ReplicaId uint64 `protobuf:"varint,2,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
}
type PrePrepare struct {
	//视图
	View uint64 `protobuf:"varint,1,opt,name=view" json:"view,omitempty"`
	//序列号
	SequenceNumber uint64 `protobuf:"varint,2,opt,name=sequence_number,json=sequenceNumber" json:"sequence_number,omitempty"`
	//批消息摘要
	BatchDigest string `protobuf:"bytes,3,opt,name=batch_digest,json=batchDigest" json:"batch_digest,omitempty"`
	//批消息
	RequestBatch *RequestBatch `protobuf:"bytes,4,opt,name=request_batch,json=requestBatch" json:"request_batch,omitempty"`
	//节点ID
	ReplicaId uint64 `protobuf:"varint,5,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
}
type Prepare struct {
	//视图
	View uint64 `protobuf:"varint,1,opt,name=view" json:"view,omitempty"`
	//序列号
	SequenceNumber uint64 `protobuf:"varint,2,opt,name=sequence_number,json=sequenceNumber" json:"sequence_number,omitempty"`
	//批消息摘要
	BatchDigest string `protobuf:"bytes,3,opt,name=batch_digest,json=batchDigest" json:"batch_digest,omitempty"`
	//节点ID
	ReplicaId uint64 `protobuf:"varint,4,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
}

type Commit struct {
	//视图
	View uint64 `protobuf:"varint,1,opt,name=view" json:"view,omitempty"`
	//序列号
	SequenceNumber uint64 `protobuf:"varint,2,opt,name=sequence_number,json=sequenceNumber" json:"sequence_number,omitempty"`
	//批消息摘要
	BatchDigest string `protobuf:"bytes,3,opt,name=batch_digest,json=batchDigest" json:"batch_digest,omitempty"`
	//节点ID
	ReplicaId uint64 `protobuf:"varint,4,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
}
type ViewChange_C struct {
	//序列号
	SequenceNumber uint64 `protobuf:"varint,1,opt,name=sequence_number,json=sequenceNumber" json:"sequence_number,omitempty"`
	//节点ID
	Id string `protobuf:"bytes,3,opt,name=id" json:"id,omitempty"`
}
type ViewChange_PQ struct {
	//序列号
	SequenceNumber uint64 `protobuf:"varint,1,opt,name=sequence_number,json=sequenceNumber" json:"sequence_number,omitempty"`
	//批消息摘要
	BatchDigest string `protobuf:"bytes,2,opt,name=batch_digest,json=batchDigest" json:"batch_digest,omitempty"`
	//视图
	View uint64 `protobuf:"varint,3,opt,name=view" json:"view,omitempty"`
}

type Checkpoint struct {
	//序列号
	SequenceNumber uint64 `protobuf:"varint,1,opt,name=sequence_number,json=sequenceNumber" json:"sequence_number,omitempty"`
	//节点ID
	ReplicaId uint64 `protobuf:"varint,2,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
	//消息ID
	Id string `protobuf:"bytes,3,opt,name=id" json:"id,omitempty"`
}

type ViewChange struct {
	//当前视图
	View uint64 `protobuf:"varint,1,opt,name=view" json:"view,omitempty"`
	//高水位
	H uint64 `protobuf:"varint,2,opt,name=h" json:"h,omitempty"`
	//C集合
	Cset []*ViewChange_C `protobuf:"bytes,3,rep,name=cset" json:"cset,omitempty"`
	//P集合
	Pset []*ViewChange_PQ `protobuf:"bytes,4,rep,name=pset" json:"pset,omitempty"`
	//Q集合
	Qset []*ViewChange_PQ `protobuf:"bytes,5,rep,name=qset" json:"qset,omitempty"`
	//节点ID
	ReplicaId uint64 `protobuf:"varint,6,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
	//指纹签名
	Signature []byte `protobuf:"bytes,7,opt,name=signature,proto3" json:"signature,omitempty"`
}
type NewView struct {
	//当前视图
	View uint64 `protobuf:"varint,1,opt,name=view" json:"view,omitempty"`
	//V集合
	Vset []*ViewChange `protobuf:"bytes,2,rep,name=vset" json:"vset,omitempty"`
	//X集合
	Xset map[uint64]string `protobuf:"bytes,3,rep,name=xset" json:"xset,omitempty" protobuf_key:"varint,1,opt,name=key" protobuf_val:"bytes,2,opt,name=value"`
	//节点ID
	ReplicaId uint64 `protobuf:"varint,4,opt,name=replica_id,json=replicaId" json:"replica_id,omitempty"`
}
type PQset struct {
	Set []*ViewChange_PQ `protobuf:"bytes,1,rep,name=set" json:"set,omitempty"`
}
