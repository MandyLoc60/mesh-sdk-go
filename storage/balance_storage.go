// Copyright 2020 Coinbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"

	"github.com/coinbase/rosetta-sdk-go/asserter"
	"github.com/coinbase/rosetta-sdk-go/parser"
	"github.com/coinbase/rosetta-sdk-go/reconciler"
	"github.com/coinbase/rosetta-sdk-go/types"
	"github.com/coinbase/rosetta-sdk-go/utils"
)

var _ BlockWorker = (*BalanceStorage)(nil)

const (
	// balanceNamespace is prepended to any stored balance.
	balanceNamespace = "balance"
)

var (
	errAccountMissing = errors.New("account missing")
	errAccountFound   = errors.New("account found")
)

/*
  Key Construction
*/

// GetBalanceKey returns a deterministic hash of an types.Account + types.Currency.
func GetBalanceKey(account *types.AccountIdentifier, currency *types.Currency) []byte {
	return []byte(
		fmt.Sprintf("%s/%s/%s", balanceNamespace, types.Hash(account), types.Hash(currency)),
	)
}

// GetBalanceKeyHistorical returns a deterministic hash of an types.Account + types.Currency + block index.
func GetBalanceKeyHistorical(account *types.AccountIdentifier, currency *types.Currency, blockIndex int64) []byte {
	return []byte(
		fmt.Sprintf("%s/%s/%s/%020d", balanceNamespace, types.Hash(account), types.Hash(currency), blockIndex),
	)
}

// BalanceStorageHandler is invoked after balance changes are committed to the database.
type BalanceStorageHandler interface {
	BlockAdded(ctx context.Context, block *types.Block, changes []*parser.BalanceChange) error
	BlockRemoved(ctx context.Context, block *types.Block, changes []*parser.BalanceChange) error
}

// BalanceStorageHelper functions are used by BalanceStorage to process balances. Defining an
// interface allows the client to determine if they wish to query the node for
// certain information or use another datastore.
type BalanceStorageHelper interface {
	AccountBalance(
		ctx context.Context,
		account *types.AccountIdentifier,
		currency *types.Currency,
		block *types.BlockIdentifier,
	) (*types.Amount, error)

	ExemptFunc() parser.ExemptOperation
	BalanceExemptions() []*types.BalanceExemption
	Asserter() *asserter.Asserter
}

// BalanceStorage implements block specific storage methods
// on top of a Database and DatabaseTransaction interface.
type BalanceStorage struct {
	db      Database
	helper  BalanceStorageHelper
	handler BalanceStorageHandler

	parser *parser.Parser
}

// NewBalanceStorage returns a new BalanceStorage.
func NewBalanceStorage(
	db Database,
) *BalanceStorage {
	return &BalanceStorage{
		db: db,
	}
}

// Initialize adds a BalanceStorageHelper and BalanceStorageHandler to BalanceStorage.
// This must be called prior to syncing!
func (b *BalanceStorage) Initialize(
	helper BalanceStorageHelper,
	handler BalanceStorageHandler,
) {
	b.helper = helper
	b.handler = handler
	b.parser = parser.New(
		helper.Asserter(),
		helper.ExemptFunc(),
		helper.BalanceExemptions(),
	)
}

// AddingBlock is called by BlockStorage when adding a block to storage.
func (b *BalanceStorage) AddingBlock(
	ctx context.Context,
	block *types.Block,
	transaction DatabaseTransaction,
) (CommitWorker, error) {
	changes, err := b.parser.BalanceChanges(ctx, block, false)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to calculate balance changes", err)
	}

	for _, change := range changes {
		if err := b.UpdateBalance(ctx, transaction, change, block.ParentBlockIdentifier); err != nil {
			return nil, err
		}
	}

	return func(ctx context.Context) error {
		return b.handler.BlockAdded(ctx, block, changes)
	}, nil
}

// RemovingBlock is called by BlockStorage when removing a block from storage.
func (b *BalanceStorage) RemovingBlock(
	ctx context.Context,
	block *types.Block,
	transaction DatabaseTransaction,
) (CommitWorker, error) {
	changes, err := b.parser.BalanceChanges(ctx, block, true)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to calculate balance changes", err)
	}

	for _, change := range changes {
		if err := b.UpdateBalance(ctx, transaction, change, block.BlockIdentifier); err != nil {
			return nil, err
		}
	}

	return func(ctx context.Context) error {
		return b.handler.BlockRemoved(ctx, block, changes)
	}, nil
}

type balanceEntry struct {
	Account        *types.AccountIdentifier `json:"account"`
	Amount         *types.Amount            `json:"amount"`
	Block          *types.BlockIdentifier   `json:"block"`
	LastReconciled *types.BlockIdentifier   `json:"last_reconciled"`
}

// SetBalance allows a client to set the balance of an account in a database
// transaction (removing all historical states). This is particularly useful
// for bootstrapping balances.
func (b *BalanceStorage) SetBalance(
	ctx context.Context,
	dbTransaction DatabaseTransaction,
	account *types.AccountIdentifier,
	amount *types.Amount,
	block *types.BlockIdentifier,
) error {
	// Remove all historical records
	if err := b.removeHistoricalBalances(
		ctx,
		dbTransaction,
		account,
		amount.Currency,
		-1,
	); err != nil {
		return err
	}

	serialBal, err := b.db.Encoder().Encode(balanceNamespace, balanceEntry{
		Account: account,
		Amount:  amount,
		Block:   block,
	})
	if err != nil {
		return err
	}

	// Set current record
	key := GetBalanceKey(account, amount.Currency)
	if err := dbTransaction.Set(ctx, key, serialBal, false); err != nil {
		return err
	}

	// Set historical record
	key = GetBalanceKeyHistorical(account, amount.Currency, block.Index)
	if err := dbTransaction.Set(ctx, key, serialBal, true); err != nil {
		return err
	}

	return nil
}

// Reconciled updates the LastReconciled field on a particular
// balance. Tracking reconciliation coverage is an important
// end condition.
func (b *BalanceStorage) Reconciled(
	ctx context.Context,
	account *types.AccountIdentifier,
	currency *types.Currency,
	block *types.BlockIdentifier,
) error {
	dbTransaction := b.db.NewDatabaseTransaction(ctx, true)
	defer dbTransaction.Discard(ctx)

	key := GetBalanceKey(account, currency)
	exists, balance, err := dbTransaction.Get(ctx, key)
	if err != nil {
		return fmt.Errorf(
			"%w: unable to get balance entry for account %s:%s",
			err,
			types.PrettyPrintStruct(account),
			types.PrettyPrintStruct(currency),
		)
	}

	if !exists {
		return fmt.Errorf(
			"balance entry is missing for account %s:%s",
			types.PrettyPrintStruct(account),
			types.PrettyPrintStruct(currency),
		)
	}

	var bal balanceEntry
	if err := b.db.Encoder().Decode(balanceNamespace, balance, &bal, true); err != nil {
		return fmt.Errorf("%w: unable to decode balance entry", err)
	}

	// Don't update last reconciled if the most recent reconciliation was
	// lower than the last reconciliation. This can occur when inactive
	// reconciliation gets ahead of the active reconciliation backlog.
	if bal.LastReconciled != nil && bal.LastReconciled.Index > block.Index {
		return nil
	}

	bal.LastReconciled = block

	serialBal, err := b.db.Encoder().Encode(balanceNamespace, bal)
	if err != nil {
		return fmt.Errorf("%w: unable to encod balance entry", err)
	}

	if err := dbTransaction.Set(ctx, key, serialBal, true); err != nil {
		return fmt.Errorf("%w: unable to set balance entry", err)
	}

	if err := dbTransaction.Commit(ctx); err != nil {
		return fmt.Errorf("%w: unable to commit last reconciliation update", err)
	}

	return nil
}

// ReconciliationCoverage returns the proportion of accounts [0.0, 1.0] that
// have been reconciled at an index >= to a minimumIndex.
func (b *BalanceStorage) ReconciliationCoverage(
	ctx context.Context,
	minimumIndex int64,
) (float64, error) {
	seen := 0
	validCoverage := 0
	err := b.getAllBalanceEntries(ctx, func(entry balanceEntry) {
		seen++
		if entry.LastReconciled == nil {
			return
		}

		if entry.LastReconciled.Index >= minimumIndex {
			validCoverage++
		}
	})
	if err != nil {
		return -1, fmt.Errorf("%w: unable to get all balance entries", err)
	}

	if seen == 0 {
		return 0, nil
	}

	return float64(validCoverage) / float64(seen), nil
}

// existingValue finds the existing value for
// a given *types.AccountIdentifier and *types.Currency.
func (b *BalanceStorage) existingValue(
	ctx context.Context,
	change *parser.BalanceChange,
	parentBlock *types.BlockIdentifier,
	balEntry balanceEntry,
	exists bool,
	exemptions []*types.BalanceExemption,
) (string, error) {
	// Don't attempt to use the helper if we are going to query the same
	// block we are processing (causes the duplicate issue).
	//
	// We also ensure we don't exit with 0 if the value already exists,
	// which could be true if balances are bootstrapped.
	if !exists && parentBlock != nil && change.Block.Hash == parentBlock.Hash {
		return "0", nil
	}

	var existingValue string
	if exists {
		existingValue = balEntry.Amount.Value

		// We can exit with the existing value if there are no
		// applicable exemptions.
		if len(exemptions) == 0 {
			return existingValue, nil
		}
	}

	// Use helper to fetch existing balance.
	amount, err := b.helper.AccountBalance(ctx, change.Account, change.Currency, parentBlock)
	if err != nil {
		return "", fmt.Errorf(
			"%w: unable to get previous account balance for %s %s at %s",
			err,
			types.PrintStruct(change.Account),
			types.PrintStruct(change.Currency),
			types.PrintStruct(parentBlock),
		)
	}

	// Nothing to compare, so should return.
	if len(existingValue) == 0 {
		return amount.Value, nil
	}

	// Determine if new live balance complies
	// with any balance exemption.
	difference, err := types.SubtractValues(amount.Value, existingValue)
	if err != nil {
		return "", fmt.Errorf(
			"%w: unable to calculate difference between live and computed balances",
			err,
		)
	}

	exemption := parser.MatchBalanceExemption(
		exemptions,
		difference,
	)
	if exemption == nil {
		return "", fmt.Errorf(
			"%w: account %s balance difference (live - computed) %s at %s is not allowed by any balance exemption",
			ErrInvalidLiveBalance,
			types.PrintStruct(change.Account),
			difference,
			types.PrintStruct(parentBlock),
		)
	}

	return amount.Value, nil
}

// UpdateBalance updates a types.AccountIdentifer
// by a types.Amount and sets the account's most
// recent accessed block.
func (b *BalanceStorage) UpdateBalance(
	ctx context.Context,
	dbTransaction DatabaseTransaction,
	change *parser.BalanceChange,
	parentBlock *types.BlockIdentifier,
) error {
	if change.Currency == nil {
		return errors.New("invalid currency")
	}

	// Get existing balance on key
	key := GetBalanceKey(change.Account, change.Currency)
	exists, bal, err := dbTransaction.Get(ctx, key)
	if err != nil {
		return err
	}

	var balEntry balanceEntry
	if exists {
		err = b.db.Encoder().Decode(balanceNamespace, bal, &balEntry, true)
		if err != nil {
			return err
		}
	}

	// Find exemptions that are applicable to the *parser.BalanceChange
	exemptions := b.parser.FindExemptions(change.Account, change.Currency)

	// Find account existing value whether the account is new, has an
	// existing balance, or is subject to additional accounting from
	// a balance exemption.
	existingValue, err := b.existingValue(
		ctx,
		change,
		parentBlock,
		balEntry,
		exists,
		exemptions,
	)
	if err != nil {
		return err
	}

	newVal, err := types.AddValues(change.Difference, existingValue)
	if err != nil {
		return err
	}

	fmt.Println("new val", newVal)

	bigNewVal, ok := new(big.Int).SetString(newVal, 10)
	if !ok {
		return fmt.Errorf("%s is not an integer", newVal)
	}

	if bigNewVal.Sign() == -1 {
		return fmt.Errorf(
			"%w %s:%+v for %+v at %+v",
			ErrNegativeBalance,
			newVal,
			change.Currency,
			change.Account,
			change.Block,
		)
	}

	// Remove any historical state balances keys >= change.BlockIndex
	var orphanEvent bool
	if exists && balEntry.Block.Index >= change.Block.Index {
		fmt.Println("orphan event", types.PrintStruct(balEntry), change.Block.Index)
		orphanEvent = true
		if err := b.removeHistoricalBalances(
			ctx,
			dbTransaction,
			change.Account,
			change.Currency,
			change.Block.Index,
		); err != nil {
			return err
		}
	}

	newEntry := balanceEntry{
		Account: change.Account,
		Amount: &types.Amount{
			Value:    newVal,
			Currency: change.Currency,
		},
		Block: change.Block,
	}

	// Add a new historical record if balance change is not
	// the result of an orphan.
	if !orphanEvent {
		serialBal, err := b.db.Encoder().Encode(balanceNamespace, newEntry)
		if err != nil {
			return err
		}

		historicalKey := GetBalanceKeyHistorical(change.Account, change.Currency, change.Block.Index)
		if err := dbTransaction.Set(ctx, historicalKey, serialBal, true); err != nil {
			return err
		}
	}

	// Add last reconciled to newly created entry
	if balEntry.LastReconciled != nil {
		newEntry.LastReconciled = balEntry.LastReconciled
	}

	serialBal, err := b.db.Encoder().Encode(balanceNamespace, newEntry)
	if err != nil {
		return err
	}

	if err := dbTransaction.Set(ctx, key, serialBal, true); err != nil {
		return err
	}

	return nil
}

// GetBalance returns all the balances of a types.AccountIdentifier
// and the types.BlockIdentifier it was last updated at.
func (b *BalanceStorage) GetBalance(
	ctx context.Context,
	account *types.AccountIdentifier,
	currency *types.Currency,
	block *types.BlockIdentifier,
) (*types.Amount, error) {
	// We use a write-ready transaction here in case we need to
	// inject a non-existent balance into storage.
	dbTx := b.db.NewDatabaseTransaction(ctx, true)
	defer dbTx.Discard(ctx)

	amount, err := b.GetBalanceTransactional(ctx, dbTx, account, currency, block)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to get balance", err)
	}

	// We commit any changes made during the balance lookup.
	if err := dbTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("%w: unable to commit account balance transaction", err)
	}

	return amount, nil
}

// GetBalanceTransactional returns all the balances of a types.AccountIdentifier
// and the types.BlockIdentifier it was last updated at in a database transaction.
func (b *BalanceStorage) GetBalanceTransactional(
	ctx context.Context,
	dbTx DatabaseTransaction,
	account *types.AccountIdentifier,
	currency *types.Currency,
	block *types.BlockIdentifier,
) (*types.Amount, error) {
	// TODO: if block > head block, should return an error

	key := GetBalanceKey(account, currency)
	exists, _, err := dbTx.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	// When beginning syncing from an arbitrary height, an account may
	// not yet have a cached balance when requested. If this is the case,
	// we fetch the balance from the node for the given height and persist
	// it. This is particularly useful when monitoring interesting accounts.
	if !exists {
		fmt.Println("doesn't exist")
		amount, err := b.helper.AccountBalance(ctx, account, currency, block)
		if err != nil {
			return nil, fmt.Errorf("%w: unable to get account balance from helper", err)
		}

		err = b.SetBalance(
			ctx,
			dbTx,
			account,
			amount,
			block,
		)
		if err != nil {
			return nil, fmt.Errorf("%w: unable to set account balance", err)
		}

		return amount, nil
	}

	amount, _, err := b.getHistoricalBalance(
		ctx,
		dbTx,
		account,
		currency,
		block,
	)
	// If account record exists but we don't
	// find any records for the index, we assume
	// the balance to be 0 (i.e. before any balance
	// changes applied). If syncing starts after
	// genesis, this behavior could cause issues.
	if errors.Is(err, errAccountMissing) {
		return &types.Amount{
			Value:    "0",
			Currency: currency,
		}, nil
	}
	if err != nil {
		return nil, err
	}

	fmt.Println("historical balance", amount)
	return amount, nil
}

// BootstrapBalance represents a balance of
// a *types.AccountIdentifier and a *types.Currency in the
// genesis block.
type BootstrapBalance struct {
	Account  *types.AccountIdentifier `json:"account_identifier,omitempty"`
	Currency *types.Currency          `json:"currency,omitempty"`
	Value    string                   `json:"value,omitempty"`
}

// BootstrapBalances is utilized to set the balance of
// any number of AccountIdentifiers at the genesis blocks.
// This is particularly useful for setting the value of
// accounts that received an allocation in the genesis block.
func (b *BalanceStorage) BootstrapBalances(
	ctx context.Context,
	bootstrapBalancesFile string,
	genesisBlockIdentifier *types.BlockIdentifier,
) error {
	// Read bootstrap file
	balances := []*BootstrapBalance{}
	if err := utils.LoadAndParse(bootstrapBalancesFile, &balances); err != nil {
		return err
	}

	// Update balances in database
	dbTransaction := b.db.NewDatabaseTransaction(ctx, true)
	defer dbTransaction.Discard(ctx)

	for _, balance := range balances {
		// Ensure change.Difference is valid
		amountValue, ok := new(big.Int).SetString(balance.Value, 10)
		if !ok {
			return fmt.Errorf("%s is not an integer", balance.Value)
		}

		if amountValue.Sign() < 1 {
			return fmt.Errorf("cannot bootstrap zero or negative balance %s", amountValue.String())
		}

		log.Printf(
			"Setting account %s balance to %s %+v\n",
			balance.Account.Address,
			balance.Value,
			balance.Currency,
		)

		err := b.SetBalance(
			ctx,
			dbTransaction,
			balance.Account,
			&types.Amount{
				Value:    balance.Value,
				Currency: balance.Currency,
			},
			genesisBlockIdentifier,
		)
		if err != nil {
			return err
		}
	}

	err := dbTransaction.Commit(ctx)
	if err != nil {
		return err
	}

	log.Printf("%d Balances Bootstrapped\n", len(balances))
	return nil
}

func (b *BalanceStorage) getAllBalanceEntries(
	ctx context.Context,
	handler func(balanceEntry),
) error {
	namespace := balanceNamespace
	txn := b.db.NewDatabaseTransaction(ctx, false)
	defer txn.Discard(ctx)
	_, err := txn.Scan(
		ctx,
		[]byte(namespace),
		func(k []byte, v []byte) error {
			var deserialBal balanceEntry
			// We should not reclaim memory during a scan!!
			err := b.db.Encoder().Decode(namespace, v, &deserialBal, false)
			if err != nil {
				return fmt.Errorf(
					"%w: unable to parse balance entry for %s",
					err,
					string(v),
				)
			}

			handler(deserialBal)
			return nil
		},
		false,
		false,
	)
	if err != nil {
		return fmt.Errorf("%w: database scan failed", err)
	}

	return nil
}

// GetAllAccountCurrency scans the db for all balances and returns a slice
// of reconciler.AccountCurrency. This is useful for bootstrapping the reconciler
// after restart.
func (b *BalanceStorage) GetAllAccountCurrency(
	ctx context.Context,
) ([]*reconciler.AccountCurrency, error) {
	log.Println("Loading previously seen accounts (this could take a while)...")

	balances := []*balanceEntry{}
	if err := b.getAllBalanceEntries(ctx, func(entry balanceEntry) {
		balances = append(balances, &entry)
	}); err != nil {
		return nil, fmt.Errorf("%w: unable to get all balance entries", err)
	}

	accounts := make([]*reconciler.AccountCurrency, len(balances))
	for i, balance := range balances {
		accounts[i] = &reconciler.AccountCurrency{
			Account:  balance.Account,
			Currency: balance.Amount.Currency,
		}
	}

	return accounts, nil
}

// SetBalanceImported sets the balances of a set of addresses by
// getting their balances from the tip block, and populating the database.
// This is used when importing prefunded addresses.
func (b *BalanceStorage) SetBalanceImported(
	ctx context.Context,
	helper BalanceStorageHelper,
	accountBalances []*utils.AccountBalance,
) error {
	// Update balances in database
	transaction := b.db.NewDatabaseTransaction(ctx, true)
	defer transaction.Discard(ctx)

	for _, accountBalance := range accountBalances {
		log.Printf(
			"Setting account %s balance to %s %+v\n",
			accountBalance.Account.Address,
			accountBalance.Amount.Value,
			accountBalance.Amount.Currency,
		)

		err := b.SetBalance(
			ctx,
			transaction,
			accountBalance.Account,
			accountBalance.Amount,
			accountBalance.Block,
		)
		if err != nil {
			return err
		}
	}

	if err := transaction.Commit(ctx); err != nil {
		return err
	}

	log.Printf("%d Balances Updated\n", len(accountBalances))
	return nil
}

// getHistoricalBalance returns the balance of an account
// at a particular *types.BlockIdentifier.
func (b *BalanceStorage) getHistoricalBalance(
	ctx context.Context,
	dbTx DatabaseTransaction,
	account *types.AccountIdentifier,
	currency *types.Currency,
	block *types.BlockIdentifier,
) (*types.Amount, *types.BlockIdentifier, error) {
	var foundAmount *types.Amount
	var foundBlock *types.BlockIdentifier
	_, err := dbTx.Scan(
		ctx,
		GetBalanceKeyHistorical(account, currency, block.Index),
		func(k []byte, v []byte) error {
			fmt.Println(string(k))
			var deserialBal balanceEntry
			// We should not reclaim memory during a scan!!
			err := b.db.Encoder().Decode(balanceNamespace, v, &deserialBal, false)
			if err != nil {
				return fmt.Errorf(
					"%w: unable to parse balance entry for %s",
					err,
					string(v),
				)
			}

			// Ensure block hash matches in case of orphan
			if deserialBal.Block.Index == block.Index && deserialBal.Block.Hash != block.Hash {
				return fmt.Errorf(
					"wanted block identifier %s but got %s",
					types.PrintStruct(block),
					types.PrintStruct(deserialBal.Block),
				)
			}

			foundAmount = deserialBal.Amount
			foundBlock = deserialBal.Block
			return errAccountFound
		},
		false,
		true,
	)
	if errors.Is(err, errAccountFound) {
		return foundAmount, foundBlock, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: database scan failed", err)
	}

	return nil, nil, errAccountMissing
}

// removeHistoricalBalances deletes all historical balances
// >= a particular index (used during reorg).
func (b *BalanceStorage) removeHistoricalBalances(
	ctx context.Context,
	dbTx DatabaseTransaction,
	account *types.AccountIdentifier,
	currency *types.Currency,
	index int64,
) error {
	foundKeys := [][]byte{}
	_, err := dbTx.Scan(
		ctx,
		GetBalanceKeyHistorical(account, currency, index),
		func(k []byte, v []byte) error {
			thisK := make([]byte, len(k))
			copy(thisK, k)

			foundKeys = append(foundKeys, thisK)
			return nil
		},
		false,
		false,
	)
	if err != nil {
		return fmt.Errorf("%w: database scan failed", err)
	}

	for _, k := range foundKeys {
		if err := dbTx.Delete(ctx, k); err != nil {
			return err
		}
	}

	return nil
}
