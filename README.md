# TrustNet TEE Builder

TrustNet TEE Builder is a BNB Smart Chain (BSC) compatible builder node that implements a PBS/MEV bundle auction and is designed to run inside a Trusted Execution Environment (TEE).

## BNB Smart Chain

TrustNet TEE Builder is based on the official BNB Smart Chain client and inherits its consensus, networking and JSON-RPC behavior. For MEV/PBS design and general BSC background, please refer to the upstream builder README and related documentation.

### Upstream BSC builder documentation (appendix)

For generic BSC PBS builder design, MEV background and integration details, please refer to the original BSC builder repository:

- https://github.com/bnb-chain/bsc-builder/blob/main/README.md

The rest of this README focuses on TrustNet TEE Builder-specific behavior and TEE deployment.

## TrustNet Builder Guide 

### 1. Feature Overview

#### 1.1 PBS-based builder and ordering algorithm

- This project is a fork of BNB Smart Chain / go-ethereum and implements a PBS/MEV builder compatible with BSC.
- When `Eth.Miner.Mev.BuilderEnabled = true` in the config, the node will, on each block it produces, select a winning private bundle from the bundle pool and include it in the block.
- The selection logic for private bundles is:
  - Read all candidate bundles from the pending bundle pool.
  - For each bundle, run a full EVM simulation of all included transactions.
  - Compute:
    - `gasFees`: total gas fees from all transactions in the bundle (including blob gas if any).
    - `bribe`: sum of `value` of all transactions whose `to` is `BuilderControlEOA`.
    - `score = gasFees + bribe`: the combined score used for ordering.
  - Filter out bundles whose bribe is lower than `MinBribe`.
  - For the remaining candidates, sort descending by `score`, then by `bribe`, then by `bundleHash` and pick the first as `winner` and the second as `second`.
- After the winner is selected, the node will:
  - Commit the winner bundle’s transactions into the block.
  - Add both the gas revenue and the bribe paid to the block profit.
  - Record this auction result into an in-memory cache.

`recordPrivateBundleAuction` builds a `PrivateBundleAuction` structure that:

- Stores `BlockNumber`, `ParentHash` and the hashes of the winning and second bundles.
- Stores `ScoreWinner` / `ScoreSecond` and `BribeWinner` / `BribeSecond`.
- Computes `RefundTotal = max(winner.bribe - second.bribe, 0)`.
- Fills `WinnerBribeBySender`, showing the exact bribe contributed by each address sending funds to `BuilderControlEOA`.
- Inserts it into a miner-side LRU cache in memory.

Overall this implements a “gas fees + bribe” scoring and a second-price-like refund behavior on top of BSC blocks.

#### 1.2 TEE-specific deployment traits

TrustNet Builder is designed to run inside a TEE (Trusted Execution Environment) to protect:

- The contents of private transaction bundles during execution and ordering from leakage to the host machine.
- The ordering and auction algorithm from being tampered with, by running it inside a trusted execution environment.
- Builder keys, password files and configuration from being exposed outside the enclave.

### 2. Build and Run

#### 2.1 Build binaries locally

- Build `geth` from this repository:

  ```shell
  make geth
  # or
  go run build/ci.go install -static ./cmd/geth
  ./build/bin/geth version
  ```

#### 2.2 Build Docker images

- Multi-stage build directly from this source:

  ```shell
  docker build --pull -t trustnet-builder:latest -f Dockerfile .
  ```

- If the default base images cannot be pulled, you can specify mirrors:

  ```shell
  docker build \
    --build-arg GO_BASE_IMAGE=dockerproxy.com/library/golang:1.24-alpine \
    --build-arg RUNTIME_BASE_IMAGE=dockerproxy.com/library/alpine:3.21 \
    -t trustnet-builder:latest -f Dockerfile .
  ```

- Tag and export the image for distribution:

  ```shell
  make docker-tag-trustnet
  make docker-save-trustnet
  # Output: ./trustnet-builder.tar
  ```

- Load and verify on the target host:

  ```shell
  sudo docker load -i trustnet-builder.tar
  sudo docker run --rm trustnet-builder:latest geth version
  ```

#### 2.3 Run with Docker Compose

Fixed directory conventions:

- Mainnet:
  - Config: `/tmp/bsc/config/mainnet`
  - Data: `/tmp/bsc/data/mainnet`
  - Password: `/tmp/bsc/secret/password.txt`
- Testnet:
  - Config: `/tmp/bsc/config/testnet`
  - Data: `/tmp/bsc/data/testnet`
  - Password: `/tmp/bsc/secret/password.txt`

Prepare directories and files:

```shell
sudo mkdir -p /tmp/bsc/config/{mainnet,testnet} /tmp/bsc/data/{mainnet,testnet} /tmp/bsc/secret
sudo chown -R 1000:1000 /tmp/bsc/data/{mainnet,testnet}
cp -v ./configs/testnet/config.pruned.toml /tmp/bsc/config/testnet/config.toml
cp -v ./configs/testnet/genesis.json /tmp/bsc/config/testnet/genesis.json
cp -v ./password.txt /tmp/bsc/secret/password.txt
```

For mainnet, copy a mainnet config template to `/tmp/bsc/config/mainnet/config.toml` and prepare the corresponding `genesis.json`.

Run on testnet:

```shell
docker compose -f docker-compose.testnet.yml up -d
```

- Volume mappings:
  - `/tmp/bsc/config/testnet` → `/bsc/config`
  - `/tmp/bsc/data/testnet` → `/data`
  - `/tmp/bsc/secret/password.txt` → `/bsc/password.txt`
- Exposed ports:
  - HTTP/WS RPC: `8575/8576`
  - Metrics: `6061`
  - P2P: `35555/tcp+udp`, `30303/30311`

Run on mainnet (if `docker-compose.yaml` is prepared):

```shell
docker compose -f docker-compose.yaml up -d
```

Container entry behavior:

- Reads `Node.DataDir` from `/bsc/config/config.toml` (recommended to set it to `/data`).
- If the data directory does not contain a `geth` subdirectory, it will first run a one-time `init` using `/bsc/config/genesis.json`, then start the node.

#### 2.4 Run inside a TEE (high-level)

Running inside a TEE typically involves preparing a trusted dataset (snapshot, configs, keystore and secrets), loading the `trustnet-builder` image inside the enclave environment, and starting the node with a non-interactive script that handles `init` and `start`. The exact flow depends on your TEE platform and deployment tooling and is therefore left to the operator.

### 3. Configuration (MEV-related)

The default testnet configuration is at `configs/testnet/config.toml`. General
chain, txpool and node options (`[Eth]`, `[Eth.Miner]`, `[Eth.TxPool]`,
`[Eth.GPO]`, `[Node]`, `[Node.P2P]`, `[Node.LogConfig]` etc.) follow the
upstream BSC builder and are not repeated here.

#### 3.1 MEV / builder config `[Eth.Miner.Mev]`

The most important options for TrustNet TEE Builder are:

- `BuilderEnabled = true`
- `BuilderAccount = "0x00011b01928e3c1b361dd2ff4b07ee361de70307"`
- `MinBribe = "1000000000000000"`
- `BuilderControlEOA = "0xb407870f1ae4abac6a27020019dc96365c32f1fd"`

A minimal example:

```toml
[Eth.Miner.Mev]
BuilderEnabled = true
BuilderAccount = "0x00011b01928e3c1b361dd2ff4b07ee361de70307"
MinBribe = "1000000000000000"
BuilderControlEOA = "0xb407870f1ae4abac6a27020019dc96365c32f1fd"
```

Meaning:

- `BuilderEnabled`: enable private bundle building and auction logic.
- `BuilderAccount`: receives gas fees and effective bribes from winning bundles.
- `MinBribe`: minimum bribe threshold; bundles below this value are filtered out during auction.
- `BuilderControlEOA`: control address that receives bribes; transfers to this address are aggregated as `bribe` in the bundle simulation.

`[[Eth.Miner.Mev.Validators]]`:

- Configure builder endpoints for validators:
  - `Address`: validator address.
  - `URL`: MEV/PBS endpoint (e.g. official BSC testnet/private endpoints).

### 4. APIs: send_Bundle and mev_privateBundleAuction

This section documents two commonly used APIs for private transaction handling.

#### 4.1 send_Bundle (`bundle_sendBundle`)

On this node, `SendBundle` is exposed as part of the `bundle` namespace:

- JSON-RPC method name: `bundle_sendBundle`
- Params: a single `SendBundleArgs` object
- Return: bundle hash (`common.Hash`)

`SendBundleArgs` (JSON view):

```json
{
  "txs": ["0x...", "0x..."],
  "maxBlockNumber": 0,
  "minTimestamp": 0,
  "maxTimestamp": 0,
  "revertingTxHashes": ["0x...", "0x..."],
  "droppingTxHashes": ["0x...", "0x..."]
}
```

Field semantics:

- `txs`: ordered list of RLP-encoded signed transactions (hex string with `0x` prefix).
- `maxBlockNumber`:
  - If non-zero, it is the last block number in which the bundle may be included.
  - If zero and `maxTimestamp` is not set, the server will automatically set `MaxBlockNumber` to `currentBlock + MaxBundleAliveBlock`.
- `minTimestamp` / `maxTimestamp`:
  - Unit is seconds (same as the block header `Time`).
  - If both are zero, the server will set `maxTimestamp` to `currentBlockTime + MaxBundleAliveTime`.
  - If both are set, the node will enforce `maxTimestamp > minTimestamp`, not earlier than current block time, and not later than `currentBlockTime + MaxBundleAliveTime`.
- `revertingTxHashes`:
  - These transactions may fail without causing the entire bundle to be treated as failed.
- `droppingTxHashes`:
  - Transactions that fail in execution or receipt and belong to `droppingTxHashes` will be dropped from the bundle while the rest continues.

If parameters are invalid, the node returns JSON-RPC error code `-38000` (`InvalidBundleParamError`).

Example JSON-RPC call:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "bundle_sendBundle",
  "params": [
    {
      "txs": ["0xf86b808504a817c80082520894...", "0xf86b018504a817c80082520894..."],
      "maxBlockNumber": 0,
      "minTimestamp": 0,
      "maxTimestamp": 0,
      "revertingTxHashes": [],
      "droppingTxHashes": []
    }
  ]
}
```

If you use the Go SDK provided in this repo (`ethclient.Client`), you can call the helper:

```go
hash, err := client.SendBundle(ctx, args)
```

This helper currently uses the method name `eth_sendBundle` to remain compatible with some ecosystems. In deployment you can route this to your node’s `bundle_sendBundle` via a proxy or adjust the method name accordingly.

#### 4.2 mev_privateBundleAuction

JSON-RPC method:

- Name: `mev_privateBundleAuction`
- Namespace: `mev`
- Param: a single `bundleHash` (`0x...`)
- Return:
  - If found: a `PrivateBundleAuctionResult` object
  - If not found: `null`

`PrivateBundleAuctionResult` structure:

```json
{
  "blockNumber": "0x...",
  "parentHash": "0x...",
  "winnerBundle": "0x...",
  "secondBundle": "0x...",
  "scoreWinner": "0x...",
  "scoreSecond": "0x...",
  "bribeWinner": "0x...",
  "bribeSecond": "0x...",
  "refundTotal": "0x...",
  "winnerBribeBySender": {
    "0xSender1": "0x...",
    "0xSender2": "0x..."
  },
  "createdAt": 1730000000000
}
```

Field semantics:

- `blockNumber`: block number where the auction happened (hex string).
- `parentHash`: parent block hash of the produced block.
- `winnerBundle`: hash of the private bundle that was finally included in the block.
- `secondBundle`: hash of the bundle with the second highest score (if any).
- `scoreWinner` / `scoreSecond`: scores of the respective bundles, i.e. `gasFees + bribe`.
- `bribeWinner` / `bribeSecond`: total bribe amounts for the winner and the runner-up.
- `refundTotal`: `max(bribeWinner - bribeSecond, 0)`, representing the refundable part of the bribe.
- `winnerBribeBySender`: per-sender breakdown of bribes to `BuilderControlEOA`.
- `createdAt`: timestamp when this record was created inside the node (milliseconds).

Internal behavior:

- When building a block, the node writes the winner and second bundle info into an internal `PrivateBundleAuction` structure and stores it in an in-memory LRU cache maintained by the miner.
- When queried, the miner reads from this cache and the MEV RPC layer converts it into the public `PrivateBundleAuctionResult` structure.
- The implementation only keeps a limited number of recent auction records in memory and does not persist them to an external database; after a restart or once evicted by the LRU policy, a historical `bundleHash` will yield `null`.

Example call:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "mev_privateBundleAuction",
  "params": ["0x<bundle-hash>"]
}
```

返回示例（有记录）：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "blockNumber": "0x2820c4",
    "parentHash": "0x...",
    "winnerBundle": "0x...",
    "secondBundle": "0x...",
    "scoreWinner": "0x...",
    "scoreSecond": "0x...",
    "bribeWinner": "0x...",
    "bribeSecond": "0x...",
    "refundTotal": "0x...",
    "winnerBribeBySender": {
      "0xabc...": "0x...",
      "0xdef...": "0x..."
    },
    "createdAt": 1730000000000
  }
}
```
