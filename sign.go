package pbft

//实现了 pbftCore 对象中关于消息签名和验证方面的功能，
//以及 ViewChange 对象中关于 id、设置签名及验证签名的功能。
import pb "github.com/golang/protobuf/proto"

type signable interface {
	getSignature() []byte
	setSignature(s []byte)
	getID() uint64
	setID(id uint64)
	serialize() ([]byte, error)
}

//签名
func (instance *pbftCore) sign(s signable) error {
	s.setSignature(nil)
	raw, err := s.serialize()
	if err != nil {
		return err
	}
	signedRaw, err := instance.consumer.sign(raw)
	if err != nil {
		return err
	}
	s.setSignature(signedRaw)

	return nil
}

//验证
func (instance *pbftCore) verify(s signable) error {
	origSig := s.getSignature()
	s.setSignature(nil)
	raw, err := s.serialize()
	s.setSignature(origSig)
	if err != nil {
		return err
	}
	return instance.consumer.verify(s.getID(), origSig, raw)
}

func (vc *ViewChange) getSignature() []byte {
	return vc.Signature
}

func (vc *ViewChange) setSignature(sig []byte) {
	vc.Signature = sig
}

func (vc *ViewChange) getID() uint64 {
	return vc.ReplicaId
}

func (vc *ViewChange) setID(id uint64) {
	vc.ReplicaId = id
}

func (vc *ViewChange) serialize() ([]byte, error) {
	return pb.Marshal(vc)
}
