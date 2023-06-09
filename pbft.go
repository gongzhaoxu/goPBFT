package pbft

//实现了 PBFT 对外的创建方法：New 和 GetPlugin。
import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyperledger/fabric/consensus"
	pb "github.com/hyperledger/fabric/protos"

	"github.com/golang/protobuf/proto"
	"github.com/spf13/viper"
)

const configPrefix = "CORE_PBFT"

var pluginInstance consensus.Consenter
var config *viper.Viper

func init() {
	config = loadConfig()
}

// GetPlugin 将句柄返回给 Consenter 单例
func GetPlugin(c consensus.Stack) consensus.Consenter {
	if pluginInstance == nil {
		pluginInstance = New(c)
	}
	return pluginInstance
}

// New 创建一个提供 Consenter 接口的新 Obc* 实例。
func New(stack consensus.Stack) consensus.Consenter {
	handle, _, _ := stack.GetNetworkHandles()
	id, _ := getValidatorID(handle)

	switch strings.ToLower(config.GetString("general.mode")) {
	case "batch":
		return newObcBatch(id, config, stack)
	default:
		panic(fmt.Errorf("Invalid PBFT mode: %s", config.GetString("general.mode")))
	}
}

func loadConfig() (config *viper.Viper) {
	config = viper.New()

	// for environment variables
	config.SetEnvPrefix(configPrefix)
	config.AutomaticEnv()
	replacer := strings.NewReplacer(".", "_")
	config.SetEnvKeyReplacer(replacer)

	config.SetConfigName("config")
	config.AddConfigPath("./")
	config.AddConfigPath("../consensus/pbft/")
	config.AddConfigPath("../../consensus/pbft")
	// 基于GOPATH查找配置文件的路径
	gopath := os.Getenv("GOPATH")
	for _, p := range filepath.SplitList(gopath) {
		pbftpath := filepath.Join(p, "src/github.com/hyperledger/fabric/consensus/pbft")
		config.AddConfigPath(pbftpath)
	}

	err := config.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("Error reading %s plugin config: %s", configPrefix, err))
	}
	return
}

type obcGeneric struct {
	stack consensus.Stack
	pbft  *pbftCore
}

func (op *obcGeneric) skipTo(seqNo uint64, id []byte, replicas []uint64) {
	info := &pb.BlockchainInfo{}
	err := proto.Unmarshal(id, info)
	if err != nil {
		logger.Error(fmt.Sprintf("Error unmarshaling: %s", err))
		return
	}
	op.stack.UpdateState(&checkpointMessage{seqNo, id}, info, getValidatorHandles(replicas))
}

func (op *obcGeneric) invalidateState() {
	op.stack.InvalidateState()
}

func (op *obcGeneric) validateState() {
	op.stack.ValidateState()
}

func (op *obcGeneric) getState() []byte {
	return op.stack.GetBlockchainInfoBlob()
}

func (op *obcGeneric) getLastSeqNo() (uint64, error) {
	raw, err := op.stack.GetBlockHeadMetadata()
	if err != nil {
		return 0, err
	}
	meta := &Metadata{}
	proto.Unmarshal(raw, meta)
	return meta.SeqNo, nil
}
