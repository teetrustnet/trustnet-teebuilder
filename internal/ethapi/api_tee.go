package ethapi

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type TeeAPI struct {
	b Backend
}

func NewTeeAPI(b Backend) *TeeAPI {
	return &TeeAPI{b}
}

func (s *TeeAPI) SendBundle(ctx context.Context, args types.SendBundleArgs) (common.Hash, error) {
	if len(args.Txs) == 0 {
		return common.Hash{}, newBundleError(errors.New("bundle missing txs"))
	}

	currentHeader := s.b.CurrentHeader()

	if args.MaxBlockNumber == 0 && (args.MaxTimestamp == nil || *args.MaxTimestamp == 0) {
		maxTimeStamp := currentHeader.Time + types.MaxBundleAliveTime
		args.MaxTimestamp = &maxTimeStamp
	}

	if args.MaxBlockNumber != 0 && args.MaxBlockNumber > currentHeader.Number.Uint64()+types.MaxBundleAliveBlock {
		return common.Hash{}, newBundleError(errors.New("the maxBlockNumber should not be lager than currentBlockNum + 100"))
	}

	if args.MaxTimestamp != nil && args.MinTimestamp != nil && *args.MaxTimestamp != 0 && *args.MinTimestamp != 0 {
		if *args.MaxTimestamp <= *args.MinTimestamp {
			return common.Hash{}, newBundleError(errors.New("the maxTimestamp should not be less than minTimestamp"))
		}
	}

	if args.MaxTimestamp != nil && *args.MaxTimestamp != 0 && *args.MaxTimestamp < currentHeader.Time {
		return common.Hash{}, newBundleError(errors.New("the maxTimestamp should not be less than currentBlockTimestamp"))
	}

	if (args.MaxTimestamp != nil && *args.MaxTimestamp > currentHeader.Time+types.MaxBundleAliveTime) ||
		(args.MinTimestamp != nil && *args.MinTimestamp > currentHeader.Time+types.MaxBundleAliveTime) {
		return common.Hash{}, newBundleError(errors.New("the minTimestamp/maxTimestamp should not be later than currentBlockTimestamp + 5 minutes"))
	}

	var txs types.Transactions
	for _, encodedTx := range args.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(encodedTx); err != nil {
			return common.Hash{}, err
		}
		txs = append(txs, tx)
	}

	var minTimestamp, maxTimestamp uint64
	if args.MinTimestamp != nil {
		minTimestamp = *args.MinTimestamp
	}
	if args.MaxTimestamp != nil {
		maxTimestamp = *args.MaxTimestamp
	}

	bundle := &types.Bundle{
		Txs:               txs,
		MaxBlockNumber:    args.MaxBlockNumber,
		MinTimestamp:      minTimestamp,
		MaxTimestamp:      maxTimestamp,
		RevertingTxHashes: args.RevertingTxHashes,
		DroppingTxHashes:  args.DroppingTxHashes,
	}
	if bundle.MaxBlockNumber == 0 && bundle.MaxTimestamp == 0 {
		bundle.MaxBlockNumber = currentHeader.Number.Uint64() + types.MaxBundleAliveBlock
	}

	err := s.b.SendBundle(ctx, bundle)
	if err != nil {
		return common.Hash{}, err
	}
	return bundle.Hash(), nil
}

func (s *TeeAPI) BundleAuction(_ context.Context, bundleHash common.Hash) (*BundleAuctionResult, error) {
	info := s.b.PrivateBundleAuction(bundleHash)
	if info == nil {
		return nil, nil
	}
	return toBundleAuctionResult(info), nil
}
