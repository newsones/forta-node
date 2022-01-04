package ethereum

import (
	"context"
	"math/big"
	"time"

	"github.com/forta-protocol/forta-node/utils"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	log "github.com/sirupsen/logrus"
)

const (
	maxNonceDrift = 50
)

// ContractBackend is the same interface.
type ContractBackend interface {
	bind.ContractBackend
}

// contractBackend is a wrapper of go-ethereum client. This is useful for implementing
// extra features. It's not thread-safe.
type contractBackend struct {
	localNonce      uint64
	lastServerNonce uint64

	gasPrice        *big.Int
	gasPriceUpdated time.Time
	maxPrice        *big.Int

	ContractBackend
}

// NewContractBackend creates a new contract backend by wrapping `ethclient.Client`.
func NewContractBackend(client *rpc.Client, maxPrice *big.Int) bind.ContractBackend {
	return &contractBackend{
		ContractBackend: ethclient.NewClient(client),
		maxPrice:        maxPrice,
	}
}

// SuggestGasPrice retrieves the currently suggested gas price and adds 10%
func (cb *contractBackend) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	if cb.gasPrice != nil && time.Since(cb.gasPriceUpdated) < 1*time.Minute {
		return cb.gasPrice, nil
	}
	gp, err := cb.ContractBackend.SuggestGasPrice(ctx)
	if err != nil {
		return nil, err
	}
	utils.AddPercentage(gp, 10)
	if cb.maxPrice != nil {
		if gp.Cmp(cb.maxPrice) == 1 {
			log.WithFields(log.Fields{
				"suggested": gp.Int64(),
				"maximum":   cb.maxPrice.Int64(),
			}).Warn("returning maximum price")
			return cb.maxPrice, nil
		}
	}
	//TODO: drop to debug
	log.WithFields(log.Fields{
		"gasPrice": gp.Int64(),
	}).Info("returning gas price")
	cb.gasPriceUpdated = time.Now()
	cb.gasPrice = gp
	return gp, nil
}

// PendingNonceAt helps us count the nonce more robustly.
func (cb *contractBackend) PendingNonceAt(ctx context.Context, account common.Address) (pendingNonce uint64, err error) {
	logger := log.WithField("address", account.Hex())
	cb.lastServerNonce, err = cb.ContractBackend.PendingNonceAt(ctx, account)
	if err != nil {
		logger.WithError(err).Error("failed to get pending nonce from server")
		return 0, err
	}
	logger = logger.WithFields(log.Fields{
		"serverNonce": cb.lastServerNonce,
		"localNonce":  cb.localNonce,
	})
	switch {
	case cb.localNonce == 0:
		logger.Info("using server nonce (first time)")
		return cb.lastServerNonce, nil

	case cb.localNonce > cb.lastServerNonce && cb.localNonce-cb.lastServerNonce >= maxNonceDrift:
		logger.Warn("resetted local nonce")
		cb.resetNonce()
		return cb.lastServerNonce, nil

	default:
		logger.Info("using local nonce")
		return cb.localNonce, nil
	}
}

const (
	errStrReplacementTx = "replacement transaction underpriced"
)

func isReplacementErr(err error) bool {
	return err.Error() == errStrReplacementTx
}

// SendTransaction sends the transaction with the most up-to-date nonce.
func (cb *contractBackend) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	logger := getTxLogger(tx)
	logger.Info("sending")
	if err := cb.ContractBackend.SendTransaction(ctx, tx); err != nil {
		// quickly go back to the last server nonce when the error repeats
		if isReplacementErr(err) {
			cb.resetNonce()
		}
		logger.WithError(err).Error("failed to send")
		return err
	}
	logger.Info("sent")
	// count it locally: if sending the tx is successful than that's the previous nonce for sure
	cb.incrementNonce(tx)
	return nil
}

func (cb *contractBackend) incrementNonce(tx *types.Transaction) {
	newNonce := tx.Nonce() + 1
	if newNonce > cb.localNonce {
		cb.localNonce = newNonce
	}
}

func (cb *contractBackend) resetNonce() {
	if cb.lastServerNonce < cb.localNonce {
		cb.localNonce = cb.lastServerNonce
	}
}

func getTxLogger(tx *types.Transaction) *log.Entry {
	return log.WithFields(log.Fields{
		"to":       tx.To().Hex(),
		"nonce":    tx.Nonce(),
		"gasLimit": tx.Gas(),
		"gasPrice": tx.GasPrice().Uint64(),
		"hash":     tx.Hash().Hex(),
	})
}
