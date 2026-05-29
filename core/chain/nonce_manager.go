package chain

// core/chain/nonce_manager.go
//
// Thread-safe nonce manager that tracks the next nonce to use for each sender.
// Prevents nonce gaps when multiple goroutines submit transactions concurrently.
//
// Usage:
//   nm := NewNonceManager(client)
//   nonce, err := nm.Next(ctx, walletAddr)
//   // ... build & sign tx with this nonce ...
//   err = nm.SendTx(ctx, signedTx)
//   // On success: nonce consumed, next call returns nonce+1
//   // On failure: nonce is released back to the pool

import (
	"context"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

type NonceManager struct {
	client *ethclient.Client
	mu     sync.Mutex
	// Map of sender address → next nonce to use
	nonces map[common.Address]uint64
	// Set of in-flight nonces per address to avoid reuse
	inFlight map[common.Address]map[uint64]bool
}

func NewNonceManager(client *ethclient.Client) *NonceManager {
	return &NonceManager{
		client:   client,
		nonces:   make(map[common.Address]uint64),
		inFlight: make(map[common.Address]map[uint64]bool),
	}
}

// Next returns the next nonce for the given address, fetching from chain on first call.
func (nm *NonceManager) Next(ctx context.Context, addr common.Address) (uint64, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	// Initialize from chain on first use
	if _, ok := nm.nonces[addr]; ! ok {
		pending, err := nm.client.PendingNonceAt(ctx, addr)
		if err != nil {
			return 0, fmt.Errorf("fetch nonce for %s: %w", addr.Hex(), err)
		}
		nm.nonces[addr] = pending
		nm.inFlight[addr] = make(map[uint64]bool)
	}

	n := nm.nonces[addr]
	nm.inFlight[addr][n] = true
	nm.nonces[addr] = n + 1
	return n, nil
}

// Release returns a nonce to the pool if tx failed (so it can be reused).
func (nm *NonceManager) Release(addr common.Address, nonce uint64) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if flights, ok := nm.inFlight[addr]; ok {
		if flights[nonce] {
			delete(flights, nonce)
			// If nonce is below current next, reset next to this nonce
			if nonce < nm.nonces[addr] {
				nm.nonces[addr] = nonce
			}
		}
	}
}

// Confirm marks a nonce as used on-chain (removed from in-flight set).
func (nm *NonceManager) Confirm(addr common.Address, nonce uint64) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if flights, ok := nm.inFlight[addr]; ok {
		delete(flights, nonce)
	}
}

// SendTx signs and sends a transaction using managed nonce.
func (nm *NonceManager) SendTx(ctx context.Context, addr common.Address, tx *types.Transaction) error {
	err := nm.client.SendTransaction(ctx, tx)
	if err != nil {
		// Release nonce on failure so it can be reused
		nm.Release(addr, tx.Nonce())
		return err
	}
	// Nonce is confirmed once tx is in mempool
	nm.Confirm(addr, tx.Nonce())
	return nil
}
