# TrustNet TeeBuilder

**TEE-native block building for BNB Chain.**

TrustNet TeeBuilder is the execution client + builder stack that moves block building into a Trusted Execution Environment (TEE), turning encrypted order flow into verifiable, privacy-preserving execution.

This README is based on TrustNet’s public technical article:
- https://x.com/TrustNetTEE/status/1989349599249912277

---

## Why this exists

MEV on BNB Chain is structurally under-addressed. Traditional approaches (first-price builder markets, soft coordination, or copied Ethereum/Solana designs) are not a direct fit for BNB’s PoSA model and fast block cadence.

TrustNet’s thesis:
1. If order flow is visible before execution, extractive MEV is inevitable.
2. TEE must be integrated into the block-building path, not bolted on as a superficial privacy layer.
3. Commodity TEE hardware is not sufficient for blockchain-grade high concurrency.

---

## What TrustNet TeeBuilder does

### 1) Encrypted order flow by default
Transactions/bundles are processed inside TEE enclaves so intent is not leaked before execution.

### 2) Verifiable execution
Execution outputs and inclusion decisions are tied to remote attestation and verifiable roots.

### 3) Fairer auction mechanism
Supports sealed-bid / second-price style auction primitives to reduce strategy leakage and winner-takes-all centralization pressure.

### 4) Validator-compatible integration on BNB
Designed for BNB Chain’s consensus and timing constraints, with a practical path for validator adoption.

---

## High-level architecture

```text
[Users / Searchers]
      ↓ encrypted txs/bundles
[TrustNet Relay]
      ↓
[SPU TEE Cluster: decrypt | simulate | auction | order]
      ↓
[Inclusion artifacts + attestation]
      ↓
[Validator: verify | merge | propose]
      ↓
[BNB Chain execution]
```

---

## Why custom SPU instead of commodity TEE

TrustNet uses **SPU (Secured Processing Unit)** hardware architecture for blockchain execution workloads:
- Better scaling path for memory / IO under high throughput
- Reduced coupling to shared CPU microarchitecture assumptions
- Designed for sustained low-latency, high-concurrency block-building workloads
- Foundation for heterogeneous confidential compute (CPU + accelerator paths)

---

## Milestones

- ✅ TrustNet has demonstrated TEE-built block production in BNB environments.
- ✅ First TEE-produced block in BNB Chain history (as announced publicly), validated by HashGlobal: block `#88246239`.

Reference:
- https://x.com/TrustNetTEE/status/2036335664632045611

---

## Repository scope

This repository contains TrustNet TeeBuilder code and related BNB execution/client components used to prototype and validate:
- TEE-enforced ordering and block construction
- Verifiable inclusion and attestation-linked execution paths
- Builder-side auction / relay logic and validator integration

---

## Status

This project is under active iteration and research-driven development.
Treat all code as experimental unless explicitly marked production-ready.

---

## Additional context

- If order flow remains visible pre-execution, MEV extraction persists.
- TEE alone is not enough; economic design and verifier pathways matter.
- TrustNet focuses on combining hardware, protocol, and incentive design into one end-to-end execution pipeline.
