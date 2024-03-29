// Package broadcaster contains code for interacting with the decentr blockchain.
package broadcaster

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/pflag"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/tendermint/spm/cosmoscmd"

	"github.com/Decentr-net/decentr/app"
	"github.com/Decentr-net/decentr/config"
)

func init() {
	config.SetAddressPrefixes()
}

// ErrTxInMempoolCache is returned when tx is already broadcast and exists in mempool cache.
var ErrTxInMempoolCache = errors.New("tx is already in mempool cache")

//go:generate mockgen -destination=./mock/broadcaster.go -package=mock -source=blockchain.go

// Broadcaster provides functionality to broadcast messages to cosmos based blockchain node.
type Broadcaster interface {
	// From returns address of broadcaster.
	From() sdk.AccAddress
	// GetHeight returns current height.
	GetHeight(ctx context.Context) (uint64, error)
	// BroadcastMsg broadcasts alone message.
	BroadcastMsg(msg sdk.Msg, memo string) (*sdk.TxResponse, error)
	// Broadcast broadcasts messages.
	Broadcast(msgs []sdk.Msg, memo string) (*sdk.TxResponse, error)

	// PingContext pings node.
	PingContext(ctx context.Context) error
}

var accountSequenceMismatchErrorRegExp = regexp.MustCompile(`.+account sequence mismatch, expected (\d+), got \d+:.+`)

type broadcaster struct {
	ctx client.Context
	txf tx.Factory

	mu sync.Mutex
}

// Config ...
type Config struct {
	KeyringRootDir     string
	KeyringBackend     string
	KeyringPromptInput string

	NodeURI       string
	BroadcastMode string

	From    string
	ChainID string

	Fees      sdk.Coins
	Gas       uint64
	GasAdjust float64
}

// New returns new instance of broadcaster
func New(cfg Config) (*broadcaster, error) {
	kr, err := keyring.New(
		config.AppName,
		cfg.KeyringBackend,
		cfg.KeyringRootDir,
		strings.NewReader(cfg.KeyringPromptInput),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	acc, err := kr.Key(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	c, err := client.NewClientFromNode(cfg.NodeURI)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	encodingConfig := cosmoscmd.MakeEncodingConfig(app.ModuleBasics)
	ctx := client.Context{}.
		WithCodec(encodingConfig.Marshaler).
		WithChainID(cfg.ChainID).
		WithInterfaceRegistry(encodingConfig.InterfaceRegistry).
		WithTxConfig(encodingConfig.TxConfig).
		WithLegacyAmino(encodingConfig.Amino).
		WithAccountRetriever(types.AccountRetriever{}).
		WithBroadcastMode(cfg.BroadcastMode).
		WithHomeDir(cfg.KeyringRootDir).
		WithKeyring(kr).
		WithFrom(acc.GetName()).
		WithFromName(acc.GetName()).
		WithFromAddress(acc.GetAddress()).
		WithNodeURI(cfg.NodeURI).
		WithClient(c)

	factory := tx.NewFactoryCLI(ctx, &pflag.FlagSet{}).
		WithFees(cfg.Fees.String()).
		WithGas(cfg.Gas).
		WithGasAdjustment(cfg.GasAdjust)

	b := &broadcaster{
		ctx: ctx,
		txf: factory,

		mu: sync.Mutex{},
	}

	if err := b.refreshSequence(); err != nil {
		return nil, fmt.Errorf("failed to refresh sequence: %w", err)
	}

	return b, nil
}

// From returns address of broadcaster.
func (b *broadcaster) From() sdk.AccAddress {
	return b.ctx.FromAddress
}

// GetHeight returns current height.
func (b *broadcaster) GetHeight(ctx context.Context) (uint64, error) {
	c, err := b.ctx.GetNode()
	if err != nil {
		return 0, fmt.Errorf("failed get node: %w", err)
	}

	i, err := c.ABCIInfo(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch ABCIInfo: %w", err)
	}

	return uint64(i.Response.LastBlockHeight), nil
}

// BroadcastMsg broadcasts alone message.
func (b *broadcaster) BroadcastMsg(msg sdk.Msg, memo string) (*sdk.TxResponse, error) {
	return b.Broadcast([]sdk.Msg{msg}, memo)
}

// Broadcast broadcasts messages.
func (b *broadcaster) Broadcast(msgs []sdk.Msg, memo string) (*sdk.TxResponse, error) {
	out, err := b.broadcast(msgs, memo, false)

	if err != nil {
		return nil, fmt.Errorf("failed to broadcast: %w", err)
	}

	return out, nil
}

// PingContext pings node.
func (b *broadcaster) PingContext(ctx context.Context) error {
	c, err := b.ctx.GetNode()
	if err != nil {
		return fmt.Errorf("failed to get rpc client: %w", err)
	}
	if _, err := c.ABCIInfo(ctx); err != nil {
		return fmt.Errorf("failed to check node status: %w", err)
	}

	return nil
}

func (b *broadcaster) broadcast(msgs []sdk.Msg, memo string, isRetry bool) (*sdk.TxResponse, error) {
	if !isRetry {
		b.mu.Lock()
		defer b.mu.Unlock()
	}

	txf := b.txf.WithMemo(memo)

	if txf.GasAdjustment() == 0 {
		txf = txf.WithGasAdjustment(1)
	}

	if txf.Gas() == 0 {
		_, gas, err := tx.CalculateGas(b.ctx, txf, msgs...)
		if err != nil {
			if !isRetry {
				if seq := getNextSequence(err.Error()); seq != 0 {
					b.txf = b.txf.WithSequence(seq)
				}

				return b.broadcast(msgs, memo, true)
			}

			return nil, fmt.Errorf("failed to calculate gas: %w", err)
		}
		txf = txf.WithGas(gas)
	}

	unsignedTx, err := tx.BuildUnsignedTx(txf, msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to build tx: %w", err)
	}

	if err := tx.Sign(txf, b.ctx.GetFromName(), unsignedTx, true); err != nil {
		return nil, fmt.Errorf("failed to sign tx: %w", err)
	}

	txBytes, err := b.ctx.TxConfig.TxEncoder()(unsignedTx.GetTx())
	if err != nil {
		return nil, fmt.Errorf("failed to encode tx: %w", err)
	}

	// broadcast to a Tendermint node
	resp, err := b.ctx.BroadcastTx(txBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to broadcast tx: %w", err)
	}

	if resp.Code != 0 {
		if sdkerrors.ErrTxInMempoolCache.ABCICode() == resp.Code {
			return nil, ErrTxInMempoolCache
		}

		if !isRetry {
			if seq := getNextSequence(resp.RawLog); seq != 0 {
				b.txf = b.txf.WithSequence(seq)
			}

			return b.broadcast(msgs, memo, true)
		}

		return nil, fmt.Errorf("failed to broadcast tx: %s", resp.String())
	}

	b.txf = b.txf.WithSequence(b.txf.Sequence() + 1)

	return resp, nil
}

func (b *broadcaster) refreshSequence() error {
	if err := b.txf.AccountRetriever().EnsureExists(b.ctx, b.From()); err != nil {
		return fmt.Errorf("failed to EnsureExists: %w", err)
	}

	num, seq, err := b.txf.AccountRetriever().GetAccountNumberSequence(b.ctx, b.From())
	if err != nil {
		return fmt.Errorf("failed to get GetAccountNumberSequence: %w", err)
	}

	b.txf = b.txf.WithAccountNumber(num).WithSequence(seq)

	return nil
}

func getNextSequence(m string) uint64 {
	s := accountSequenceMismatchErrorRegExp.FindStringSubmatch(m)

	if len(s) != 2 {
		return 0
	}

	seq, _ := strconv.ParseUint(s[1], 10, 64)

	return seq
}
