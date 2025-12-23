package miner

import (
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

const smallBundleGas = 10 * params.TxGas

var (
	errNonRevertingTxInBundleFailed = errors.New("non-reverting tx in bundle failed")
	errBundlePriceTooLow            = errors.New("bundle price too low")
)

type privateBundleCandidate struct {
	bundleHash common.Hash
	txs        types.Transactions
	gasFees    *big.Int
	score      *big.Int
	bribe      *big.Int
	bribeBy    map[common.Address]*big.Int
}

type PrivateBundleAuction struct {
	BlockNumber         uint64
	ParentHash          common.Hash
	WinnerBundle        common.Hash
	SecondBundle        common.Hash
	ScoreWinner         *big.Int
	ScoreSecond         *big.Int
	BribeWinner         *big.Int
	BribeSecond         *big.Int
	RefundTotal         *big.Int
	WinnerBribeBySender map[common.Address]*big.Int
	CreatedAt           time.Time
}

func (w *worker) fillTransactionsAndBundles(interruptCh chan int32, env *environment, stopTimer *time.Timer) error {
	env.state.StopPrefetcher() // no need to prefetch txs for a builder

	// reduce gas limit for builder block
	fullGasLimit := env.header.GasLimit
	env.header.GasLimit /= 2

	defer func() {
		env.header.GasLimit = fullGasLimit
	}()

	bundles := w.eth.TxPool().PendingBundles(env.header.Number.Uint64(), env.header.Time)
	if w.config.Mev.BuilderEnabled != nil && *w.config.Mev.BuilderEnabled {
		if len(bundles) > 0 {
			winner, second, err := w.selectWinningPrivateBundle(env, bundles)
			if err != nil {
				log.Error("fail to select winning private bundle", "err", err)
				return err
			}
			if winner != nil && len(winner.txs) > 0 {
				if err := w.commitBundles(env, winner.txs, interruptCh, stopTimer); err != nil {
					log.Error("fail to commit winning private bundle", "err", err)
					return err
				}
				payBribe := winner.bribe
				if second != nil {
					payBribe = second.bribe
				}
				env.profit.Set(winner.gasFees)
				env.profit.Add(env.profit, payBribe)
				w.recordPrivateBundleAuction(env, winner, second)
				log.Info("fill private bundle", "bundles_count", len(bundles), "txs", len(winner.txs), "score", winner.score)
			}
		}

		log.Info("fill bundles done", "total_txs_count", len(env.txs))
		return nil
	}

	{
		if len(bundles) > 0 {
			txs, bundle, err := w.generateOrderedBundles(env, bundles)
			if err != nil {
				log.Error("fail to generate ordered bundles", "err", err)
				return err
			}

			if err = w.commitBundles(env, txs, interruptCh, stopTimer); err != nil {
				log.Error("test: fail to commit bundles", "err", err)
				return err
			}

			env.profit.Add(env.profit, bundle.EthSentToSystem)
			log.Info("test: fill bundles", "bundles_count", len(bundles))
		}
	}

	{
		w.confMu.RLock()
		tip := w.tip
		prio := w.prio
		w.confMu.RUnlock()

		// Retrieve the pending transactions pre-filtered by the 1559/4844 dynamic fees
		filter := txpool.PendingFilter{
			MinTip: tip,
		}
		if env.header.BaseFee != nil {
			filter.BaseFee = uint256.MustFromBig(env.header.BaseFee)
		}
		if env.header.ExcessBlobGas != nil {
			filter.BlobFee = uint256.MustFromBig(eip4844.CalcBlobFee(w.chainConfig, env.header))
		}
		filter.OnlyPlainTxs, filter.OnlyBlobTxs = true, false
		pendingPlainTxs := w.eth.TxPool().Pending(filter)

		filter.OnlyPlainTxs, filter.OnlyBlobTxs = false, true
		pendingBlobTxs := w.eth.TxPool().Pending(filter)

		// Split the pending transactions into locals and remotes.
		prioPlainTxs, normalPlainTxs := make(map[common.Address][]*txpool.LazyTransaction), pendingPlainTxs
		prioBlobTxs, normalBlobTxs := make(map[common.Address][]*txpool.LazyTransaction), pendingBlobTxs

		for _, account := range prio {
			if txs := normalPlainTxs[account]; len(txs) > 0 {
				delete(normalPlainTxs, account)
				prioPlainTxs[account] = txs
			}
			if txs := normalBlobTxs[account]; len(txs) > 0 {
				delete(normalBlobTxs, account)
				prioBlobTxs[account] = txs
			}
		}

		// Fill the block with all available pending transactions.
		if len(prioPlainTxs) > 0 || len(prioBlobTxs) > 0 {
			plainTxs := newTransactionsByPriceAndNonce(env.signer, prioPlainTxs, env.header.BaseFee)
			blobTxs := newTransactionsByPriceAndNonce(env.signer, prioBlobTxs, env.header.BaseFee)

			if err := w.commitTransactions(env, plainTxs, blobTxs, interruptCh, stopTimer); err != nil {
				return err
			}
		}
		if len(normalPlainTxs) > 0 || len(normalBlobTxs) > 0 {
			plainTxs := newTransactionsByPriceAndNonce(env.signer, normalPlainTxs, env.header.BaseFee)
			blobTxs := newTransactionsByPriceAndNonce(env.signer, normalBlobTxs, env.header.BaseFee)

			if err := w.commitTransactions(env, plainTxs, blobTxs, interruptCh, stopTimer); err != nil {
				return err
			}
		}

		log.Info("fill transactions", "plain_txs_count", len(prioPlainTxs)+len(normalPlainTxs),
			"blob_txs_count", len(prioBlobTxs)+len(normalBlobTxs))
	}

	log.Info("test: fill bundles and transactions done", "total_txs_count", len(env.txs))
	return nil
}

func (w *worker) selectWinningPrivateBundle(env *environment, bundles []*types.Bundle) (winner *privateBundleCandidate, second *privateBundleCandidate, err error) {
	minBribe := w.privateBundleMinBribe()
	control := w.config.Mev.BuilderControlEOA

	candidates := make([]*privateBundleCandidate, 0)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, bundle := range bundles {
		if bundle == nil {
			continue
		}
		wg.Add(1)
		go func(bundle *types.Bundle, state *state.StateDB) {
			defer wg.Done()
			bundleHash := bundle.Hash()
			bundleCopy := copyBundleForSimulation(bundle)

			gasPool := prepareGasPool(env.header.GasLimit)
			evm := vm.NewEVM(core.NewEVMBlockContext(env.header, w.chain, &env.coinbase), state, w.chainConfig, vm.Config{})
			gasFees, bribe, bribeBy, execErr := w.simulateBundleForPrivateAuction(evm, env.header, bundleCopy, state, gasPool, env.signer, control)
			if execErr != nil || len(bundleCopy.Txs) == 0 {
				return
			}
			if minBribe.Sign() > 0 && bribe.Cmp(minBribe) < 0 {
				return
			}
			score := new(big.Int).Add(gasFees, bribe)

			mu.Lock()
			candidates = append(candidates, &privateBundleCandidate{
				bundleHash: bundleHash,
				txs:        bundleCopy.Txs,
				gasFees:    new(big.Int).Set(gasFees),
				score:      score,
				bribe:      new(big.Int).Set(bribe),
				bribeBy:    bribeBy,
			})
			mu.Unlock()
		}(bundle, env.state.Copy())
	}
	wg.Wait()

	if len(candidates) == 0 {
		return nil, nil, nil
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score.Cmp(candidates[j].score) != 0 {
			return candidates[i].score.Cmp(candidates[j].score) > 0
		}
		if candidates[i].bribe.Cmp(candidates[j].bribe) != 0 {
			return candidates[i].bribe.Cmp(candidates[j].bribe) > 0
		}
		return candidates[i].bundleHash.Big().Cmp(candidates[j].bundleHash.Big()) > 0
	})

	winner = candidates[0]
	if len(candidates) > 1 {
		second = candidates[1]
	}
	return winner, second, nil
}

func (w *worker) privateBundleMinBribe() *big.Int {
	if w.config == nil {
		return big.NewInt(0)
	}
	if w.config.Mev.MinBribe == nil || *w.config.Mev.MinBribe == "" {
		return big.NewInt(0)
	}
	minBribe, ok := new(big.Int).SetString(*w.config.Mev.MinBribe, 10)
	if !ok {
		log.Error("failed to parse MinBribe", "MinBribe", *w.config.Mev.MinBribe)
		return big.NewInt(0)
	}
	return minBribe
}

func (w *worker) recordPrivateBundleAuction(env *environment, winner *privateBundleCandidate, second *privateBundleCandidate) {
	if w.privateBundleAuctions == nil || env == nil || env.header == nil || winner == nil {
		return
	}

	var (
		secondHash  common.Hash
		scoreSecond = big.NewInt(0)
		bribeSecond = big.NewInt(0)
		refundTotal = big.NewInt(0)
	)
	if second != nil {
		secondHash = second.bundleHash
		scoreSecond = new(big.Int).Set(second.score)
		bribeSecond = new(big.Int).Set(second.bribe)
		refundTotal = new(big.Int).Sub(winner.bribe, second.bribe)
		if refundTotal.Sign() < 0 {
			refundTotal.SetInt64(0)
		}
	}

	bribeBy := make(map[common.Address]*big.Int, len(winner.bribeBy))
	for addr, amount := range winner.bribeBy {
		if amount == nil {
			continue
		}
		bribeBy[addr] = new(big.Int).Set(amount)
	}

	r := &PrivateBundleAuction{
		BlockNumber:         env.header.Number.Uint64(),
		ParentHash:          env.header.ParentHash,
		WinnerBundle:        winner.bundleHash,
		SecondBundle:        secondHash,
		ScoreWinner:         new(big.Int).Set(winner.score),
		ScoreSecond:         scoreSecond,
		BribeWinner:         new(big.Int).Set(winner.bribe),
		BribeSecond:         bribeSecond,
		RefundTotal:         refundTotal,
		WinnerBribeBySender: bribeBy,
		CreatedAt:           time.Now(),
	}

	w.privateBundleAuctions.Add(r.WinnerBundle, r)
}

func copyBundleForSimulation(bundle *types.Bundle) *types.Bundle {
	if bundle == nil {
		return nil
	}
	cpy := *bundle
	cpy.Txs = append(types.Transactions(nil), bundle.Txs...)
	cpy.RevertingTxHashes = append([]common.Hash(nil), bundle.RevertingTxHashes...)
	cpy.DroppingTxHashes = append([]common.Hash(nil), bundle.DroppingTxHashes...)
	cpy.Price = nil
	return &cpy
}

func (w *worker) simulateBundleForPrivateAuction(
	evm *vm.EVM,
	header *types.Header,
	bundle *types.Bundle,
	state *state.StateDB,
	gasPool *core.GasPool,
	signer types.Signer,
	control common.Address,
) (gasFees *big.Int, bribe *big.Int, bribeBy map[common.Address]*big.Int, err error) {
	var (
		tempGasUsed   uint64
		totalGasFees  = new(big.Int)
		totalBribe    = new(big.Int)
		bribeBySender = make(map[common.Address]*big.Int)
		seenTxs       = make(map[common.Hash]struct{})
	)

	txsLen := len(bundle.Txs)
	for i := 0; i < txsLen; i++ {
		tx := bundle.Txs[i]
		if tx == nil {
			return nil, nil, nil, errors.New("unexpected nil transaction in bundle")
		}
		txHash := tx.Hash()
		if _, ok := seenTxs[txHash]; ok {
			continue
		}
		seenTxs[txHash] = struct{}{}

		state.SetTxContext(txHash, i)
		snap := state.Snapshot()
		gp := gasPool.Gas()

		receipt, applyErr := core.ApplyTransaction(evm, gasPool, state, header, tx, &tempGasUsed)
		if applyErr != nil {
			if containsHash(bundle.DroppingTxHashes, txHash) {
				state.RevertToSnapshot(snap)
				gasPool.SetGas(gp)
				bundle.Txs = bundle.Txs.Remove(i)
				txsLen = len(bundle.Txs)
				i--
				continue
			}
			return nil, nil, nil, applyErr
		}
		if receipt.Status == types.ReceiptStatusFailed && !containsHash(bundle.RevertingTxHashes, receipt.TxHash) {
			if containsHash(bundle.DroppingTxHashes, receipt.TxHash) {
				gasPool.SetGas(gp)
				bundle.Txs = bundle.Txs.Remove(i)
				txsLen = len(bundle.Txs)
				i--
				continue
			}
			return nil, nil, nil, errNonRevertingTxInBundleFailed
		}

		txGasUsed := new(big.Int).SetUint64(receipt.GasUsed)
		effectiveTip, tipErr := tx.EffectiveGasTip(header.BaseFee)
		if tipErr != nil {
			return nil, nil, nil, tipErr
		}
		if header.BaseFee != nil {
			effectiveTip.Add(effectiveTip, header.BaseFee)
		}
		txGasFees := new(big.Int).Mul(txGasUsed, effectiveTip)
		if tx.Type() == types.BlobTxType {
			blobFee := new(big.Int).SetUint64(receipt.BlobGasUsed)
			blobFee.Mul(blobFee, receipt.BlobGasPrice)
			txGasFees.Add(txGasFees, blobFee)
		}
		totalGasFees.Add(totalGasFees, txGasFees)

		if control != (common.Address{}) {
			to := tx.To()
			if to != nil && *to == control {
				value := tx.Value()
				if value != nil && value.Sign() > 0 {
					from, senderErr := types.Sender(signer, tx)
					if senderErr == nil {
						totalBribe.Add(totalBribe, value)
						if bribeBySender[from] == nil {
							bribeBySender[from] = new(big.Int)
						}
						bribeBySender[from].Add(bribeBySender[from], value)
					}
				}
			}
		}
	}

	if len(bundle.Txs) == 0 {
		return nil, nil, nil, errors.New("empty bundle")
	}
	return totalGasFees, totalBribe, bribeBySender, nil
}

func (w *worker) commitBundles(
	env *environment,
	txs types.Transactions,
	interruptCh chan int32,
	stopTimer *time.Timer,
) error {
	if env.gasPool == nil {
		env.gasPool = prepareGasPool(env.header.GasLimit)
	}

	var coalescedLogs []*types.Log
    // signal used to encode interrupt reason
    signal := commitInterruptNone
LOOP:
	for _, tx := range txs {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the reason is 1
		// (2) worker start or restart, the reason is 1
		// (3) worker recreate the sealing block with any newly arrived transactions, the reason is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
        if interruptCh != nil {
            select {
            case <-interruptCh:
                return errBlockInterruptedByNewHead
            default:
            }
        } // If we don't have enough gas for any further transactions then we're done
		if env.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			signal = commitInterruptOutOfGas
			break
		}
		if tx == nil {
			log.Error("Unexpected nil transaction in bundle")
            return errors.New("unexpected nil transaction in bundle")
		}
		if stopTimer != nil {
			select {
			case <-stopTimer.C:
				log.Info("Not enough time for further transactions", "txs", len(env.txs))
				stopTimer.Reset(0) // re-active the timer, in case it will be used later.
				signal = commitInterruptTimeout
				break LOOP
			default:
			}
		}

		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.chainConfig.IsEIP155(env.header.Number) {
			log.Debug("Unexpected protected transaction in bundle")
			return errors.New("unexpected protected transaction in bundle")
		}
		// Start executing the transaction
		env.state.SetTxContext(tx.Hash(), env.tcount)

		logs, err := w.commitTransaction(env, tx, core.NewReceiptBloomGenerator())
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Error("Unexpected gas limit exceeded for current block in the bundle", "sender", from)
			return errors.New("bundle commit failed: block gas limit reached")

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Error("Transaction with low nonce in the bundle", "sender", from, "nonce", tx.Nonce())
			return errors.New("bundle commit failed: nonce too low")

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Error("Account with high nonce in the bundle", "sender", from, "nonce", tx.Nonce())
			return errors.New("bundle commit failed: nonce too high")

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			continue

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
            log.Error("Transaction failed in the bundle", "hash", tx.Hash(), "err", err)
            return errors.New("bundle commit failed: tx execution error")
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
            // pending logs feed disabled in builder mode
	}
    switch signal {
    case commitInterruptNone:
        return nil
    case commitInterruptTimeout:
        return errBlockInterruptedByTimeout
    case commitInterruptOutOfGas:
        return errBlockInterruptedByOutOfGas
    default:
        return errBlockInterruptedByNewHead
    }
}

// generateOrderedBundles generates ordered txs from the given bundles.
// 1. sort bundles according to computed gas price when received.
// 2. simulate bundles based on the same state, resort.
// 3. merge resorted simulateBundles based on the iterative state.
func (w *worker) generateOrderedBundles(
	env *environment,
	bundles []*types.Bundle,
) (types.Transactions, *types.SimulatedBundle, error) {
	// sort bundles according to gas price computed when received
	sort.SliceStable(bundles, func(i, j int) bool {
		priceI, priceJ := bundles[i].Price, bundles[j].Price

		return priceI.Cmp(priceJ) >= 0
	})

	// recompute bundle gas price based on the same state and current env
	simulatedBundles, err := w.simulateBundles(env, bundles)
	if err != nil {
		log.Error("fail to simulate bundles base on the same state", "err", err)
		return nil, nil, err
	}

	// sort bundles according to fresh gas price
	sort.SliceStable(simulatedBundles, func(i, j int) bool {
		priceI, priceJ := simulatedBundles[i].BundleGasPrice, simulatedBundles[j].BundleGasPrice

		return priceI.Cmp(priceJ) >= 0
	})

	// merge bundles based on iterative state
	includedTxs, mergedBundle, err := w.mergeBundles(env, simulatedBundles)
	if err != nil {
		log.Error("fail to merge bundles", "err", err)
		return nil, nil, err
	}

	return includedTxs, mergedBundle, nil
}

func (w *worker) simulateBundles(env *environment, bundles []*types.Bundle) ([]*types.SimulatedBundle, error) {
	headerHash := env.header.Hash()
	simCache := w.bundleCache.GetBundleCache(headerHash)
	simResult := make(map[common.Hash]*types.SimulatedBundle)

	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, bundle := range bundles {
		if simmed, ok := simCache.GetSimulatedBundle(bundle.Hash()); ok {
			mu.Lock()
			simResult[bundle.Hash()] = simmed
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(idx int, bundle *types.Bundle, state *state.StateDB) {
			defer wg.Done()

			gasPool := prepareGasPool(env.header.GasLimit)
			evm := vm.NewEVM(core.NewEVMBlockContext(env.header, w.chain, &env.coinbase), state, w.chainConfig, vm.Config{})
			simmed, err := w.simulateBundle(evm, env.header, bundle, state, gasPool, 0, true, true)

			if err != nil {
				log.Trace("Error computing gas for a simulateBundle", "error", err)
				return
			}

			mu.Lock()
			defer mu.Unlock()
			simResult[bundle.Hash()] = simmed
		}(i, bundle, env.state.Copy())
	}

	wg.Wait()

	simulatedBundles := make([]*types.SimulatedBundle, 0)

	for _, bundle := range simResult {
		if bundle == nil {
			continue
		}

		simulatedBundles = append(simulatedBundles, bundle)
	}

	simCache.UpdateSimulatedBundles(simResult, bundles)

	return simulatedBundles, nil
}

// mergeBundles merges the given simulateBundle into the given environment.
// It returns the merged simulateBundle and the number of transactions that were merged.
func (w *worker) mergeBundles(
	env *environment,
	bundles []*types.SimulatedBundle,
) (types.Transactions, *types.SimulatedBundle, error) {
	currentState := env.state.Copy()
	gasPool := prepareGasPool(env.header.GasLimit)

	includedTxs := types.Transactions{}
	mergedBundle := types.SimulatedBundle{
		BundleGasFees:   new(big.Int),
		BundleGasUsed:   0,
		BundleGasPrice:  new(big.Int),
		EthSentToSystem: new(big.Int),
	}

	evm := vm.NewEVM(core.NewEVMBlockContext(env.header, w.chain, &env.coinbase), env.state.Copy(), w.chainConfig, vm.Config{})

	for _, bundle := range bundles {
		// if we don't have enough gas for any further transactions then we're done
		if gasPool.Gas() < smallBundleGas {
			break
		}

		prevState := currentState.Copy()
		prevGasPool := new(core.GasPool).AddGas(gasPool.Gas())

		// the floor gas price is 99/100 what was simulated at the top of the block
		floorGasPrice := new(big.Int).Mul(bundle.BundleGasPrice, big.NewInt(99))
		floorGasPrice = floorGasPrice.Div(floorGasPrice, big.NewInt(100))

		simulatedBundle, err := w.simulateBundle(evm, env.header, bundle.OriginalBundle, currentState, gasPool, len(includedTxs), true, false)

		if err != nil || simulatedBundle.BundleGasPrice.Cmp(floorGasPrice) <= 0 {
			currentState = prevState
			gasPool = prevGasPool

			log.Error("failed to merge bundle", "floorGasPrice", floorGasPrice.String(), "err", err)
			continue
		}

		includedTxs = append(includedTxs, bundle.OriginalBundle.Txs...)

		mergedBundle.BundleGasFees.Add(mergedBundle.BundleGasFees, simulatedBundle.BundleGasFees)
		mergedBundle.BundleGasUsed += simulatedBundle.BundleGasUsed

		for _, tx := range bundle.OriginalBundle.Txs {
			if !containsHash(bundle.OriginalBundle.RevertingTxHashes, tx.Hash()) {
				env.UnRevertible = append(env.UnRevertible, tx.Hash())
			}
		}

		log.Info("included bundle",
			"gasUsed", simulatedBundle.BundleGasUsed,
			"gasPrice", simulatedBundle.BundleGasPrice,
			"txcount", len(simulatedBundle.OriginalBundle.Txs),
			"unrevertible", len(env.UnRevertible))
	}

	if len(includedTxs) == 0 {
		return nil, nil, errors.New("include no txs when merge bundles")
	}

	mergedBundle.BundleGasPrice.Div(mergedBundle.BundleGasFees, new(big.Int).SetUint64(mergedBundle.BundleGasUsed))

	return includedTxs, &mergedBundle, nil
}

// simulateBundle computes the gas price for a whole simulateBundle based on the same ctx
// named computeBundleGas in flashbots
func (w *worker) simulateBundle(
	evm *vm.EVM, header *types.Header, bundle *types.Bundle, state *state.StateDB, gasPool *core.GasPool, currentTxCount int,
	prune, pruneGasExceed bool,
) (*types.SimulatedBundle, error) {
	var (
		tempGasUsed     uint64
		bundleGasUsed   uint64
		bundleGasFees   = new(big.Int)
		ethSentToSystem = new(big.Int)
	)

	txsLen := len(bundle.Txs)
	for i := 0; i < txsLen; i++ {
		tx := bundle.Txs[i]

		state.SetTxContext(tx.Hash(), i+currentTxCount)
		sysBalanceBefore := state.GetBalance(consensus.SystemAddress)

		snap := state.Snapshot()
		gp := gasPool.Gas()

		receipt, err := core.ApplyTransaction(evm, gasPool, state, header, tx, &tempGasUsed)

		if err != nil {
			log.Warn("fail to simulate bundle", "hash", bundle.Hash().String(), "err", err)

			if containsHash(bundle.DroppingTxHashes, tx.Hash()) {
				log.Warn("drop tx in bundle", "hash", tx.Hash().String())
				state.RevertToSnapshot(snap)
				gasPool.SetGas(gp)
				bundle.Txs = bundle.Txs.Remove(i)
				txsLen = len(bundle.Txs)
				i--
				continue
			}

			if prune {
				if errors.Is(err, core.ErrGasLimitReached) && !pruneGasExceed {
					log.Warn("bundle gas limit exceed", "hash", bundle.Hash().String())
				} else {
					log.Warn("prune bundle", "hash", bundle.Hash().String(), "err", err)
					w.eth.TxPool().PruneBundle(bundle.Hash())
				}
			}

			return nil, err
		}

		if receipt.Status == types.ReceiptStatusFailed && !containsHash(bundle.RevertingTxHashes, receipt.TxHash) {
			// for unRevertible tx but itself can be dropped, we drop it and revert the state and gas pool
			if containsHash(bundle.DroppingTxHashes, receipt.TxHash) {
				log.Warn("drop tx in bundle", "hash", receipt.TxHash.String())
				// NOTE: here should not revert state, when no err returned by ApplyTransaction, state.clearJournalAndRefund()
				// must had been called to avoid reverting across transactions, so we can directly remove the tx from bundle
				gasPool.SetGas(gp)
				bundle.Txs = bundle.Txs.Remove(i)
				txsLen = len(bundle.Txs)
				i--
				continue
			}

			err = errNonRevertingTxInBundleFailed
			log.Warn("fail to simulate bundle", "hash", bundle.Hash().String(), "err", err)

			if prune {
				w.eth.TxPool().PruneBundle(bundle.Hash())
				log.Warn("prune bundle", "hash", bundle.Hash().String())
			}

			return nil, err
		}

		if !w.eth.TxPool().Has(tx.Hash()) {
			bundleGasUsed += receipt.GasUsed

			txGasUsed := new(big.Int).SetUint64(receipt.GasUsed)
			effectiveTip, er := tx.EffectiveGasTip(header.BaseFee)
			if er != nil {
				return nil, er
			}

			if header.BaseFee != nil {
				effectiveTip.Add(effectiveTip, header.BaseFee)
			}

			txGasFees := new(big.Int).Mul(txGasUsed, effectiveTip)

			if tx.Type() == types.BlobTxType {
				blobFee := new(big.Int).SetUint64(receipt.BlobGasUsed)
				blobFee.Mul(blobFee, receipt.BlobGasPrice)
				txGasFees.Add(txGasFees, blobFee)
			}
			bundleGasFees.Add(bundleGasFees, txGasFees)
			sysBalanceAfter := state.GetBalance(consensus.SystemAddress)
			sysDelta := new(uint256.Int).Sub(sysBalanceAfter, sysBalanceBefore)
			sysDelta.Sub(sysDelta, uint256.MustFromBig(txGasFees))
			ethSentToSystem.Add(ethSentToSystem, sysDelta.ToBig())
		}
	}

	// prune bundle when all txs are dropped
	if len(bundle.Txs) == 0 {
		log.Warn("prune bundle", "hash", bundle.Hash().String(), "err", "empty bundle")
		w.eth.TxPool().PruneBundle(bundle.Hash())
		return nil, errors.New("empty bundle")
	}

	// if all txs in the bundle are from mempool, we accept the bundle without checking gas price
	bundleGasPrice := big.NewInt(0)

	if bundleGasUsed != 0 {
		bundleGasPrice = new(big.Int).Div(bundleGasFees, new(big.Int).SetUint64(bundleGasUsed))

        // accept bundles if above floor gas price (default 0)
        floor := big.NewInt(0)
        if w.config.GasPrice != nil {
            floor = w.config.GasPrice
        }
        if bundleGasPrice.Cmp(floor) < 0 {
			err := errBundlePriceTooLow
			log.Warn("fail to simulate bundle", "hash", bundle.Hash().String(), "err", err)

			if prune {
				log.Warn("prune bundle", "hash", bundle.Hash().String())
				w.eth.TxPool().PruneBundle(bundle.Hash())
			}

			return nil, err
		}
	}

	return &types.SimulatedBundle{
		OriginalBundle:  bundle,
		BundleGasFees:   bundleGasFees,
		BundleGasPrice:  bundleGasPrice,
		BundleGasUsed:   bundleGasUsed,
		EthSentToSystem: ethSentToSystem,
	}, nil
}

func (w *worker) simulateGaslessBundle(env *environment, bundle *types.Bundle) (*types.SimulateGaslessBundleResp, error) {
	validResults := make([]types.GaslessTxSimResult, 0)
	gasReachedResults := make([]types.GaslessTxSimResult, 0)

	txIdx := 0
	for _, tx := range bundle.Txs {
		env.state.SetTxContext(tx.Hash(), txIdx)

		var (
			snap = env.state.Snapshot()
			gp   = env.gasPool.Gas()
		)

		receipt, err := core.ApplyTransaction(env.evm, env.gasPool, env.state, env.header, tx, &env.header.GasUsed)
		if err != nil {
			env.state.RevertToSnapshot(snap)
			env.gasPool.SetGas(gp)
			log.Error("fail to simulate gasless tx, skipped", "hash", tx.Hash(), "err", err)

			if err == core.ErrGasLimitReached {
				gasReachedResults = append(gasReachedResults, types.GaslessTxSimResult{Hash: tx.Hash()})
			}
		} else {
			txIdx++

			validResults = append(validResults, types.GaslessTxSimResult{
				Hash:    tx.Hash(),
				GasUsed: receipt.GasUsed,
			})
		}
	}

	return &types.SimulateGaslessBundleResp{
		ValidResults:      validResults,
		GasReachedResults: gasReachedResults,
		BasedBlockNumber:  env.header.Number.Int64(),
	}, nil
}

func containsHash(arr []common.Hash, match common.Hash) bool {
	for _, elem := range arr {
		if elem == match {
			return true
		}
	}
	return false
}

func prepareGasPool(gasLimit uint64) *core.GasPool {
	gasPool := new(core.GasPool).AddGas(gasLimit)
	gasPool.SubGas(params.SystemTxsGasSoftLimit) // reserve gas for system txs(keep align with mainnet)
	return gasPool
}
