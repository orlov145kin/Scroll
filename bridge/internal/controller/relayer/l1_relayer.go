package relayer

import (
	"context"
	"errors"
	"math/big"

	// not sure if this will make problems when relay with l1geth
	"github.com/scroll-tech/go-ethereum/accounts/abi"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/crypto"
	"github.com/scroll-tech/go-ethereum/log"
	gethMetrics "github.com/scroll-tech/go-ethereum/metrics"
	"gorm.io/gorm"

	"scroll-tech/common/metrics"
	"scroll-tech/common/types"

	bridgeAbi "scroll-tech/bridge/abi"
	"scroll-tech/bridge/internal/config"
	"scroll-tech/bridge/internal/controller/sender"
	"scroll-tech/bridge/internal/orm"
)

var (
	bridgeL1MsgsRelayedTotalCounter          = gethMetrics.NewRegisteredCounter("bridge/l1/msgs/relayed/total", metrics.ScrollRegistry)
	bridgeL1MsgsRelayedConfirmedTotalCounter = gethMetrics.NewRegisteredCounter("bridge/l1/msgs/relayed/confirmed/total", metrics.ScrollRegistry)
)

// Layer1Relayer is responsible for
//  1. fetch pending L1Message from db
//  2. relay pending message to layer 2 node
//
// Actions are triggered by new head from layer 1 geth node.
// @todo It's better to be triggered by watcher.
type Layer1Relayer struct {
	ctx context.Context

	cfg *config.RelayerConfig

	// channel used to communicate with transaction sender
	messageSender  *sender.Sender
	l2MessengerABI *abi.ABI

	gasOracleSender *sender.Sender
	l1GasOracleABI  *abi.ABI

	minGasLimitForMessageRelay uint64

	lastGasPrice uint64
	minGasPrice  uint64
	gasPriceDiff uint64

	l1MessageOrm *orm.L1Message
	l1BlockOrm   *orm.L1Block
}

// NewLayer1Relayer will return a new instance of Layer1RelayerClient
func NewLayer1Relayer(ctx context.Context, db *gorm.DB, cfg *config.RelayerConfig) (*Layer1Relayer, error) {
	messageSender, err := sender.NewSender(ctx, cfg.SenderConfig, cfg.MessageSenderPrivateKeys)
	if err != nil {
		addr := crypto.PubkeyToAddress(cfg.MessageSenderPrivateKeys[0].PublicKey)
		log.Error("new MessageSender failed", "main address", addr.String(), "err", err)
		return nil, err
	}

	// @todo make sure only one sender is available
	gasOracleSender, err := sender.NewSender(ctx, cfg.SenderConfig, cfg.GasOracleSenderPrivateKeys)
	if err != nil {
		addr := crypto.PubkeyToAddress(cfg.GasOracleSenderPrivateKeys[0].PublicKey)
		log.Error("new GasOracleSender failed", "main address", addr.String(), "err", err)
		return nil, err
	}

	var minGasPrice uint64
	var gasPriceDiff uint64
	if cfg.GasOracleConfig != nil {
		minGasPrice = cfg.GasOracleConfig.MinGasPrice
		gasPriceDiff = cfg.GasOracleConfig.GasPriceDiff
	} else {
		minGasPrice = 0
		gasPriceDiff = defaultGasPriceDiff
	}

	minGasLimitForMessageRelay := uint64(defaultL1MessageRelayMinGasLimit)
	if cfg.MessageRelayMinGasLimit != 0 {
		minGasLimitForMessageRelay = cfg.MessageRelayMinGasLimit
	}

	l1Relayer := &Layer1Relayer{
		ctx:          ctx,
		l1MessageOrm: orm.NewL1Message(db),
		l1BlockOrm:   orm.NewL1Block(db),

		messageSender:  messageSender,
		l2MessengerABI: bridgeAbi.L2ScrollMessengerABI,

		gasOracleSender: gasOracleSender,
		l1GasOracleABI:  bridgeAbi.L1GasPriceOracleABI,

		minGasLimitForMessageRelay: minGasLimitForMessageRelay,

		minGasPrice:  minGasPrice,
		gasPriceDiff: gasPriceDiff,

		cfg: cfg,
	}

	go l1Relayer.handleConfirmLoop(ctx)
	return l1Relayer, nil
}

// ProcessSavedEvents relays saved un-processed cross-domain transactions to desired blockchain
func (r *Layer1Relayer) ProcessSavedEvents() {
	// msgs are sorted by nonce in increasing order
	msgs, err := r.l1MessageOrm.GetL1MessagesByStatus(types.MsgPending, 100)
	if err != nil {
		log.Error("Failed to fetch unprocessed L1 messages", "err", err)
		return
	}

	if len(msgs) > 0 {
		log.Info("Processing L1 messages", "count", len(msgs))
	}

	for _, msg := range msgs {
		tmpMsg := msg
		if err = r.processSavedEvent(&tmpMsg); err != nil {
			if !errors.Is(err, sender.ErrNoAvailableAccount) && !errors.Is(err, sender.ErrFullPending) {
				log.Error("failed to process event", "msg.msgHash", msg.MsgHash, "err", err)
			}
			return
		}
	}
}

func (r *Layer1Relayer) processSavedEvent(msg *orm.L1Message) error {
	calldata := common.Hex2Bytes(msg.Calldata)
	hash, err := r.messageSender.SendTransaction(msg.MsgHash, &r.cfg.MessengerContractAddress, big.NewInt(0), calldata, r.minGasLimitForMessageRelay)
	if err != nil && errors.Is(err, ErrExecutionRevertedMessageExpired) {
		return r.l1MessageOrm.UpdateLayer1Status(r.ctx, msg.MsgHash, types.MsgExpired)
	}

	if err != nil && errors.Is(err, ErrExecutionRevertedAlreadySuccessExecuted) {
		return r.l1MessageOrm.UpdateLayer1Status(r.ctx, msg.MsgHash, types.MsgConfirmed)
	}
	if err != nil {
		return err
	}
	bridgeL1MsgsRelayedTotalCounter.Inc(1)
	log.Info("relayMessage to layer2", "msg hash", msg.MsgHash, "tx hash", hash)

	err = r.l1MessageOrm.UpdateLayer1StatusAndLayer2Hash(r.ctx, msg.MsgHash, types.MsgSubmitted, hash.String())
	if err != nil {
		log.Error("UpdateLayer1StatusAndLayer2Hash failed", "msg.msgHash", msg.MsgHash, "msg.height", msg.Height, "err", err)
	}
	return err
}

// ProcessGasPriceOracle imports gas price to layer2
func (r *Layer1Relayer) ProcessGasPriceOracle() {
	latestBlockHeight, err := r.l1BlockOrm.GetLatestL1BlockHeight(r.ctx)
	if err != nil {
		log.Warn("Failed to fetch latest L1 block height from db", "err", err)
		return
	}

	blocks, err := r.l1BlockOrm.GetL1Blocks(r.ctx, map[string]interface{}{
		"number": latestBlockHeight,
	})
	if err != nil {
		log.Error("Failed to GetL1Blocks from db", "height", latestBlockHeight, "err", err)
		return
	}
	if len(blocks) != 1 {
		log.Error("Block not exist", "height", latestBlockHeight)
		return
	}
	block := blocks[0]

	if types.GasOracleStatus(block.GasOracleStatus) == types.GasOraclePending {
		expectedDelta := r.lastGasPrice * r.gasPriceDiff / gasPriceDiffPrecision
		// last is undefine or (block.BaseFee >= minGasPrice && exceed diff)
		if r.lastGasPrice == 0 || (block.BaseFee >= r.minGasPrice && (block.BaseFee >= r.lastGasPrice+expectedDelta || block.BaseFee <= r.lastGasPrice-expectedDelta)) {
			baseFee := big.NewInt(int64(block.BaseFee))
			data, err := r.l1GasOracleABI.Pack("setL1BaseFee", baseFee)
			if err != nil {
				log.Error("Failed to pack setL1BaseFee", "block.Hash", block.Hash, "block.Height", block.Number, "block.BaseFee", block.BaseFee, "err", err)
				return
			}

			hash, err := r.gasOracleSender.SendTransaction(block.Hash, &r.cfg.GasPriceOracleContractAddress, big.NewInt(0), data, 0)
			if err != nil {
				if !errors.Is(err, sender.ErrNoAvailableAccount) && !errors.Is(err, sender.ErrFullPending) {
					log.Error("Failed to send setL1BaseFee tx to layer2 ", "block.Hash", block.Hash, "block.Height", block.Number, "err", err)
				}
				return
			}

			err = r.l1BlockOrm.UpdateL1GasOracleStatusAndOracleTxHash(r.ctx, block.Hash, types.GasOracleImporting, hash.String())
			if err != nil {
				log.Error("UpdateGasOracleStatusAndOracleTxHash failed", "block.Hash", block.Hash, "block.Height", block.Number, "err", err)
				return
			}
			r.lastGasPrice = block.BaseFee
			log.Info("Update l1 base fee", "txHash", hash.String(), "baseFee", baseFee)
		}
	}
}

func (r *Layer1Relayer) handleConfirmLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cfm := <-r.messageSender.ConfirmChan():
			bridgeL1MsgsRelayedConfirmedTotalCounter.Inc(1)
			if !cfm.IsSuccessful {
				err := r.l1MessageOrm.UpdateLayer1StatusAndLayer2Hash(r.ctx, cfm.ID, types.MsgRelayFailed, cfm.TxHash.String())
				if err != nil {
					log.Warn("UpdateLayer1StatusAndLayer2Hash failed", "err", err)
				}
				log.Warn("transaction confirmed but failed in layer2", "confirmation", cfm)
			} else {
				// @todo handle db error
				err := r.l1MessageOrm.UpdateLayer1StatusAndLayer2Hash(r.ctx, cfm.ID, types.MsgConfirmed, cfm.TxHash.String())
				if err != nil {
					log.Warn("UpdateLayer1StatusAndLayer2Hash failed", "err", err)
				}
				log.Info("transaction confirmed in layer2", "confirmation", cfm)
			}
		case cfm := <-r.gasOracleSender.ConfirmChan():
			if !cfm.IsSuccessful {
				// @discuss: maybe make it pending again?
				err := r.l1BlockOrm.UpdateL1GasOracleStatusAndOracleTxHash(r.ctx, cfm.ID, types.GasOracleFailed, cfm.TxHash.String())
				if err != nil {
					log.Warn("UpdateL1GasOracleStatusAndOracleTxHash failed", "err", err)
				}
				log.Warn("transaction confirmed but failed in layer2", "confirmation", cfm)
			} else {
				// @todo handle db error
				err := r.l1BlockOrm.UpdateL1GasOracleStatusAndOracleTxHash(r.ctx, cfm.ID, types.GasOracleImported, cfm.TxHash.String())
				if err != nil {
					log.Warn("UpdateGasOracleStatusAndOracleTxHash failed", "err", err)
				}
				log.Info("transaction confirmed in layer2", "confirmation", cfm)
			}
		}
	}
}
