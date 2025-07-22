# Isolated RPC Node Mode

This guide explains how to configure a Tendermint node to run in "isolated mode" where it doesn't participate in peer-to-peer transaction gossip but can still serve RPC requests and process transactions submitted directly to it.

## Overview

In isolated mode, a Tendermint node:
- **Does NOT receive** transactions from other peers via p2p gossip
- **Does NOT broadcast** transactions to other peers
- **CAN still receive** transactions via RPC endpoints (like `/broadcast_tx_async`, `/broadcast_tx_sync`, `/broadcast_tx_commit`)
- **CAN still serve** RPC queries (blocks, transactions, state, etc.)
- **Maintains consensus** and stays synced with the network for blocks and state (unless app hash validation is disabled, which breaks consensus)

This mode is useful for:
- RPC-only nodes that should not participate in transaction gossip
- Nodes behind load balancers that handle transaction submission
- Testing scenarios where you want to control transaction flow
- Validator nodes that only want to receive transactions from trusted sources

## Configuration

### Basic Isolated Mode

To enable isolated mode, set the following in your `config.toml`:

```toml
[mempool]
# Disable transaction broadcasting and receiving
broadcast = false
```

### Enhanced Isolation (Optional)

For even stronger isolation, you can also configure P2P settings to limit network participation:

```toml
[p2p]
# Limit the number of peer connections
max-connections = 4

# Connect only to trusted peers (validators/full nodes you trust)
persistent-peers = "trusted_peer1@ip1:26656,trusted_peer2@ip2:26656"

# Disable peer exchange to avoid discovering random peers
pex = false

# Optional: Don't advertise your address to others
external-address = ""
```

### App Hash Validation Override (Testing Only)

For specialized testing scenarios where app hash mismatches are expected or when debugging app hash calculation issues, you can disable app hash validation:

```toml
[consensus]
# UNSAFE: Skip app hash validation during block validation
# This should only be used in testing scenarios or when debugging app hash issues
# Setting this to true will cause the node to ignore app hash mismatches with other nodes
unsafe-ignore-app-hash-validation = true
```

⚠️ **Critical Warning**: This setting should **NEVER** be used in production environments. It bypasses a critical consensus safety mechanism and will cause the node to **stop following consensus** and **fall out of sync** with the network. The node will no longer maintain consistent state with other nodes and cannot participate in consensus going forward.

### Complete Network Isolation (Testing Only)

For complete network isolation (useful for testing), you can disable all P2P connections:

```toml
[p2p]
# Disable all peer connections (node will not sync blocks!)
max-connections = 0
pex = false
persistent-peers = ""
bootstrap-peers = ""
```

⚠️ **Warning**: Complete network isolation will prevent the node from syncing blocks and maintaining consensus. Only use this for specialized testing scenarios.

## Verification

### Check Node Logs

When starting the node, you should see this log message:

```
INFO mempool running in isolated mode - transaction broadcasting and receiving disabled
```

If broadcast is enabled (normal mode), you'll see:

```
INFO mempool running in normal mode - transaction broadcasting and receiving enabled
```

### Test Transaction Submission

You can still submit transactions via RPC:

```bash
# Submit a transaction via RPC
curl -X POST http://localhost:26657/broadcast_tx_async \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc": "2.0", "id": 1, "method": "broadcast_tx_async", "params": {"tx": "your_transaction_hex"}}'
```

### Verify No P2P Transaction Gossip

In isolated mode, when other nodes send transactions to this node, you should see debug logs like:

```
DEBUG ignoring received transactions due to isolated mempool mode (broadcast=false) peer=<peer_id>
```

## Use Cases

### 1. RPC-Only Node

```toml
[mempool]
broadcast = false

[p2p]
max-connections = 8
persistent-peers = "validator1@ip1:26656,validator2@ip2:26656"
pex = false
```

### 2. Load Balancer Backend Node

```toml
[mempool]
broadcast = false

[rpc]
laddr = "tcp://0.0.0.0:26657"

[p2p]
max-connections = 4
persistent-peers = "trusted_full_node@ip:26656"
pex = false
```

### 3. Validator with Controlled Transaction Input

```toml
[mempool]
broadcast = false

[p2p]
max-connections = 20
persistent-peers = "peer1@ip1:26656,peer2@ip2:26656"
pex = true
```

### 4. Debug Node with App Hash Issues (Testing Only)

```toml
[mempool]
broadcast = false

[consensus]
# UNSAFE: Only for debugging app hash calculation issues
# WARNING: This will cause the node to fall out of consensus
unsafe-ignore-app-hash-validation = true

[p2p]
max-connections = 4
persistent-peers = "debug_peer@ip:26656"
pex = false
```

**Note**: This configuration will cause the node to stop following network consensus once an app hash mismatch occurs.

## Node Types and Isolated Mode

| Node Type | Recommended Setting | Notes |
|-----------|-------------------|-------|
| **Validator** | `broadcast = false` (optional) | Reduces noise, can still receive txs via RPC |
| **Full Node (RPC)** | `broadcast = false` | Good for dedicated RPC servers |
| **Full Node (Sync)** | `broadcast = true` | Helps network by participating in gossip |
| **Seed Node** | `broadcast = true` | Should participate in full network |
| **Sentry Node** | `broadcast = true` | Should relay transactions |

## Important Notes

1. **Block Sync Still Works**: Isolated mode only affects transaction gossip, not block synchronization
2. **Consensus Participation**: Validators in isolated mode can still participate in consensus normally
3. **RPC Functionality**: All RPC endpoints continue to work normally
4. **State Queries**: Block and state queries work normally
5. **Local Transactions**: Transactions submitted directly to this node via RPC are still processed

## Troubleshooting

### Node Not Receiving Transactions

If your isolated node isn't processing transactions:

1. Check that RPC is accessible: `curl http://localhost:26657/status`
2. Verify transactions are being submitted to the correct endpoint
3. Check mempool status: `curl http://localhost:26657/num_unconfirmed_txs`

### Node Not Staying Synced

If your node falls behind:

1. Ensure you have P2P connections: Check `/net_info` endpoint
2. Verify `persistent-peers` are reachable
3. Check that `max-connections > 0` unless completely isolated

### Transactions Not Being Included in Blocks

In isolated mode, your transactions might not propagate to validators:

1. Ensure at least one validator or well-connected node can receive your transactions
2. Consider submitting transactions to multiple nodes
3. Monitor transaction propagation using `/unconfirmed_txs` on different nodes

## Monitoring

Key metrics to monitor for isolated nodes:

- **Mempool size**: Number of transactions in local mempool
- **RPC request rate**: Incoming transaction submissions
- **Block sync status**: Ensure node stays synced
- **Peer connections**: Verify expected P2P connectivity

Example queries:

```bash
# Check mempool status
curl -s http://localhost:26657/num_unconfirmed_txs

# Check sync status
curl -s http://localhost:26657/status | jq '.result.sync_info'

# Check peer connections
curl -s http://localhost:26657/net_info | jq '.result.n_peers'
``` 