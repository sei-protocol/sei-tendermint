package mempool

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tendermint/tendermint/types"
)

func TestDuplicateTxCache(t *testing.T) {
	// Test with default TTL (0 means no expiration)
	cache := NewDuplicateTxCache(0, 0)
	require.NotNil(t, cache)

	txKey1 := types.TxKey{1, 2, 3, 4}
	txKey2 := types.TxKey{5, 6, 7, 8}

	// Test Set and Get
	cache.Set(txKey1, 5)
	if counter, found := cache.Get(txKey1); !found || counter != 5 {
		t.Errorf("Expected counter=5, found=%d, found=%v", counter, found)
	}

	// Test Increment
	newCounter := cache.Increment(txKey1)
	if newCounter != 6 {
		t.Errorf("Expected counter=6, got %d", newCounter)
	}

	// Test Increment on new key
	newCounter = cache.Increment(txKey2)
	if newCounter != 1 {
		t.Errorf("Expected counter=1, got %d", newCounter)
	}

	// Test GetTotalCounters (should only count counters > 1)
	total := cache.GetTotalCounters()
	if total != 6 { // Only txKey1 has counter > 1
		t.Errorf("Expected total=6, got %d", total)
	}

	// Test GetOneCounters (should count transactions with counter = 1)
	oneCounters := cache.GetOneCounters()
	if oneCounters != 1 { // Only txKey2 has counter = 1
		t.Errorf("Expected oneCounters=1, got %d", oneCounters)
	}

	// Test GetMaxCounter
	maxCounter := cache.GetMaxCounter()
	if maxCounter != 6 { // txKey1 has the highest counter
		t.Errorf("Expected maxCounter=6, got %d", maxCounter)
	}

	// Test Reset
	cache.Reset()
	if counter, found := cache.Get(txKey1); found {
		t.Errorf("Expected not found after reset, got counter=%d", counter)
	}

	// Test that counters are reset after Reset
	if total := cache.GetTotalCounters(); total != 0 {
		t.Errorf("Expected total=0 after reset, got %d", total)
	}
	if oneCounters := cache.GetOneCounters(); oneCounters != 0 {
		t.Errorf("Expected oneCounters=0 after reset, got %d", oneCounters)
	}
	if maxCounter := cache.GetMaxCounter(); maxCounter != 0 {
		t.Errorf("Expected maxCounter=0 after reset, got %d", maxCounter)
	}
}

func TestDuplicateTxCacheWithExpiration(t *testing.T) {
	// Test with actual TTL
	cache := NewDuplicateTxCache(100*time.Millisecond, 200*time.Millisecond)
	require.NotNil(t, cache)

	txKey := types.TxKey{1, 2, 3, 4}

	// Test Set and Get - BEFORE EXPIRY
	cache.Set(txKey, 5)
	if counter, found := cache.Get(txKey); !found || counter != 5 {
		t.Errorf("Expected counter=5 before expiry, found=%d, found=%v", counter, found)
	}

	// Test Increment - BEFORE EXPIRY
	newCounter := cache.Increment(txKey)
	if newCounter != 6 {
		t.Errorf("Expected counter=6 before expiry, got %d", newCounter)
	}

	// Test GetTotalCounters - BEFORE EXPIRY
	if total := cache.GetTotalCounters(); total != 6 {
		t.Errorf("Expected total=6 before expiry, got %d", total)
	}

	// Test GetOneCounters - BEFORE EXPIRY
	if oneCounters := cache.GetOneCounters(); oneCounters != 0 {
		t.Errorf("Expected oneCounters=0 before expiry, got %d", oneCounters)
	}

	// Test GetMaxCounter - BEFORE EXPIRY
	if maxCounter := cache.GetMaxCounter(); maxCounter != 6 {
		t.Errorf("Expected maxCounter=6 before expiry, got %d", maxCounter)
	}

	// Wait for expiration
	time.Sleep(150 * time.Millisecond)

	// Test that item has expired - AFTER EXPIRY
	if counter, found := cache.Get(txKey); found {
		t.Errorf("Expected not found after expiry, got counter=%d", counter)
	}

	// Test Increment on expired item - AFTER EXPIRY
	// Should start fresh with counter = 1
	newCounter = cache.Increment(txKey)
	if newCounter != 1 {
		t.Errorf("Expected counter=1 after expiry (fresh start), got %d", newCounter)
	}

	// Test that counters are updated after expiry - AFTER EXPIRY
	if total := cache.GetTotalCounters(); total != 0 { // counter = 1, so not counted
		t.Errorf("Expected total=0 after expiry, got %d", total)
	}
	if oneCounters := cache.GetOneCounters(); oneCounters != 1 {
		t.Errorf("Expected oneCounters=1 after expiry, got %d", oneCounters)
	}
	if maxCounter := cache.GetMaxCounter(); maxCounter != 1 {
		t.Errorf("Expected maxCounter=1 after expiry, got %d", maxCounter)
	}
}

func TestDuplicateTxCacheExpiryBehavior(t *testing.T) {
	// Test with short TTL to verify expiry behavior
	cache := NewDuplicateTxCache(50*time.Millisecond, 100*time.Millisecond)
	require.NotNil(t, cache)

	txKey1 := types.TxKey{1, 2, 3, 4}
	txKey2 := types.TxKey{5, 6, 7, 8}

	// Add transactions
	cache.Set(txKey1, 3)
	cache.Set(txKey2, 1)

	// Verify both are present - BEFORE EXPIRY
	if counter, found := cache.Get(txKey1); !found || counter != 3 {
		t.Errorf("Expected txKey1 counter=3 before expiry, found=%d, found=%v", counter, found)
	}
	if counter, found := cache.Get(txKey2); !found || counter != 1 {
		t.Errorf("Expected txKey2 counter=1 before expiry, found=%d, found=%v", counter, found)
	}

	// Verify counters - BEFORE EXPIRY
	if total := cache.GetTotalCounters(); total != 3 {
		t.Errorf("Expected total=3 before expiry, got %d", total)
	}
	if oneCounters := cache.GetOneCounters(); oneCounters != 1 {
		t.Errorf("Expected oneCounters=1 before expiry, got %d", oneCounters)
	}
	if maxCounter := cache.GetMaxCounter(); maxCounter != 3 {
		t.Errorf("Expected maxCounter=3 before expiry, got %d", maxCounter)
	}

	// Wait for expiry
	time.Sleep(75 * time.Millisecond)

	// Verify both have expired - AFTER EXPIRY
	if counter, found := cache.Get(txKey1); found {
		t.Errorf("Expected txKey1 not found after expiry, got counter=%d", counter)
	}
	if counter, found := cache.Get(txKey2); found {
		t.Errorf("Expected txKey2 not found after expiry, got counter=%d", counter)
	}

	// Verify counters are reset - AFTER EXPIRY
	if total := cache.GetTotalCounters(); total != 0 {
		t.Errorf("Expected total=0 after expiry, got %d", total)
	}
	if oneCounters := cache.GetOneCounters(); oneCounters != 0 {
		t.Errorf("Expected oneCounters=0 after expiry, got %d", oneCounters)
	}
	if maxCounter := cache.GetMaxCounter(); maxCounter != 0 {
		t.Errorf("Expected maxCounter=0 after expiry, got %d", maxCounter)
	}

	// Test that expired cache can be reused
	cache.Set(txKey1, 2)
	if counter, found := cache.Get(txKey1); !found || counter != 2 {
		t.Errorf("Expected txKey1 counter=2 after reuse, found=%d, found=%v", counter, found)
	}
}

func TestNopTxCacheWithTTL(t *testing.T) {
	// Test NOP TTL cache functionality
	cache := NopTxCacheWithTTL{}

	txKey := types.TxKey{1, 2, 3, 4}

	// Test Set (should do nothing)
	cache.Set(txKey, 5)

	// Test Get (should always return false)
	if counter, found := cache.Get(txKey); found || counter != 0 {
		t.Errorf("Expected counter=0, found=false, got counter=%d, found=%v", counter, found)
	}

	// Test Increment (should always return 1)
	if counter := cache.Increment(txKey); counter != 1 {
		t.Errorf("Expected counter=1, got %d", counter)
	}

	// Test GetTotalCounters (should always return 0)
	if total := cache.GetTotalCounters(); total != 0 {
		t.Errorf("Expected total=0, got %d", total)
	}

	// Test GetOneCounters (should always return 0)
	if oneCounters := cache.GetOneCounters(); oneCounters != 0 {
		t.Errorf("Expected oneCounters=0, got %d", oneCounters)
	}

	// Test GetMaxCounter (should always return 0)
	if maxCounter := cache.GetMaxCounter(); maxCounter != 0 {
		t.Errorf("Expected maxCounter=0, got %d", maxCounter)
	}

	// Test Reset (should do nothing)
	cache.Reset()
}

func TestDuplicateTxCacheEdgeCases(t *testing.T) {
	cache := NewDuplicateTxCache(0, 0)

	// Test with empty cache
	if total := cache.GetTotalCounters(); total != 0 {
		t.Errorf("Expected total=0 for empty cache, got %d", total)
	}
	if oneCounters := cache.GetOneCounters(); oneCounters != 0 {
		t.Errorf("Expected oneCounters=0 for empty cache, got %d", oneCounters)
	}
	if maxCounter := cache.GetMaxCounter(); maxCounter != 0 {
		t.Errorf("Expected maxCounter=0 for empty cache, got %d", maxCounter)
	}

	// Test with single transaction
	txKey := types.TxKey{1, 2, 3, 4}
	cache.Set(txKey, 1)

	if total := cache.GetTotalCounters(); total != 0 { // counter = 1, so not counted
		t.Errorf("Expected total=0 for single tx with counter=1, got %d", total)
	}
	if oneCounters := cache.GetOneCounters(); oneCounters != 1 {
		t.Errorf("Expected oneCounters=1 for single tx with counter=1, got %d", oneCounters)
	}
	if maxCounter := cache.GetMaxCounter(); maxCounter != 1 {
		t.Errorf("Expected maxCounter=1 for single tx with counter=1, got %d", maxCounter)
	}

	// Test with multiple transactions
	txKey2 := types.TxKey{5, 6, 7, 8}
	cache.Set(txKey2, 3)

	if total := cache.GetTotalCounters(); total != 3 { // only txKey2 has counter > 1
		t.Errorf("Expected total=3 for two txs, got %d", total)
	}
	if oneCounters := cache.GetOneCounters(); oneCounters != 1 { // only txKey1 has counter = 1
		t.Errorf("Expected oneCounters=1 for two txs, got %d", oneCounters)
	}
	if maxCounter := cache.GetMaxCounter(); maxCounter != 3 { // txKey2 has highest counter
		t.Errorf("Expected maxCounter=3 for two txs, got %d", maxCounter)
	}
}

func TestDuplicateTxCacheConcurrency(t *testing.T) {
	cache := NewDuplicateTxCache(0, 0)
	done := make(chan bool)
	numGoroutines := 10

	// Test concurrent access
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			txKey := types.TxKey{byte(id), byte(id + 1), byte(id + 2), byte(id + 3)}
			cache.Set(txKey, id+1)
			cache.Increment(txKey)
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify results
	expectedTotal := 0
	for i := 0; i < numGoroutines; i++ {
		expectedTotal += (i + 2) // original + 1 from increment
	}

	if total := cache.GetTotalCounters(); total != expectedTotal {
		t.Errorf("Expected total=%d, got %d", expectedTotal, total)
	}

	if maxCounter := cache.GetMaxCounter(); maxCounter != numGoroutines+1 {
		t.Errorf("Expected maxCounter=%d, got %d", numGoroutines+1, maxCounter)
	}
}
