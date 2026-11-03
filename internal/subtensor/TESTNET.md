# Proving convert on testnet

Convert has not been on chain until these steps have run. They need throwaway keys only and
testnet TAO only. `TestConvertTestnetRehearsal` pins the testnet genesis hash, so it refuses any
other chain, and it runs the same code the apps call. Keep the output of every step.

Rate limit: the public endpoints allow about 100 requests a minute per source. A rehearsal run
uses about 20.

Run everything from the sdk checkout, with its sibling repositories in place. The commands assume
`bittensor-cli` 9.23 and a zsh or bash shell. Set the phrase only in the shell that runs the test.

## 0. The runtime

```sh
SUBTENSOR_LIVE_RPC=https://test.chain.opentensor.ai go test -count=1 -run TestLiveRuntimeShape -v ./internal/subtensor
SUBTENSOR_LIVE_RPC=https://lite.chain.opentensor.ai go test -count=1 -run TestLiveRuntimeShape -v ./internal/subtensor
```

Record the spec version each prints. The sdk's fixture and source read are spec 486. A later spec
that passes means the shape it signs has not changed; this checklist still has to run for it.

## 1. Keys

```sh
btcli wallet new-coldkey --wallet-name bt-convert-a --n-words 12 --no-use-password
btcli wallet new-coldkey --wallet-name bt-convert-b --n-words 12 --no-use-password
btcli wallet new-coldkey --wallet-name bt-convert-c --n-words 12 --no-use-password
```

A pays for setup. B and C stand for a casual miner: they never receive TAO from anywhere. Note the
phrases of B and C, and the ss58 address of each key.

Fund A with at least 1 testnet TAO, from a testnet faucet or anyone who holds some.

## 2. Stake that arrives without TAO

Pick a subnet N that is not 0 and has liquidity, and H, any registered hotkey on it:

```sh
btcli subnets metagraph --network test --netuid N
```

Stake from A, then move alpha to B and C. `stake transfer` is the same `transfer_stake` the vault's
claim makes. Give B alpha worth about 0.1 TAO, and C about 0.015 TAO. The price is in the output of
step 3. Stake left worth under 0.02 TAO is sold with any sale, so B needs more than that to be left.

```sh
btcli stake add --network test --wallet-name bt-convert-a --netuid N --include-hotkeys H --amount 1 --unsafe --no-mev-protection --yes
btcli stake transfer --network test --wallet-name bt-convert-a --hotkey H --origin-netuid N --dest-netuid N --dest B_ADDRESS --amount ALPHA_FOR_B --no-mev-protection --yes
btcli stake transfer --network test --wallet-name bt-convert-a --hotkey H --origin-netuid N --dest-netuid N --dest C_ADDRESS --amount ALPHA_FOR_C --no-mev-protection --yes
```

## 3. The zero-TAO path, read-only

```sh
export BT_CONVERT_TESTNET_PHRASE='the phrase of B'
export BT_CONVERT_TESTNET_HOTKEY=H
export BT_CONVERT_TESTNET_NETUID=N
BT_CONVERT_TESTNET_AMOUNT_RAO=0 go test -count=1 -run TestConvertTestnetRehearsal -v .
```

The `before` line must show `free 0`, `nonce 0`, `providers 0`, `sufficients 0` and a stake. The
estimate must show `fee_in_alpha: true`. Take `min_amount_rao` from it, then:

```sh
BT_CONVERT_TESTNET_AMOUNT_RAO=MIN go test -count=1 -run TestConvertTestnetRehearsal -v .
```

It must print `the runtime's validation, nothing sent: valid`. That is the runtime accepting a
signed transaction from a key with no account: signature, `CheckNonce`, and the fee in alpha. Any
other answer stops here; `BadProof` means the signing payload is wrong.

## 4. The zero-TAO path, sent, at the minimum amount

```sh
BT_CONVERT_TESTNET_ACTION=convert BT_CONVERT_TESTNET_AMOUNT_RAO=MIN go test -count=1 -run TestConvertTestnetRehearsal -v .
```

Expected:

- `confirmed` shows `tao_received_rao` at least `min_tao_rao`, and `fee_alpha_rao` above 0.
- The `after` line shows `free` above 0, `nonce 1`, `providers 1`, `sufficients 1`.
- The stake fell by about the amount plus `fee_alpha_rao`.

This proves the minimum amount, and that the first TAO clears the existential deposit (500 rao, in
the estimate's reads and in `Balances.ExistentialDeposit`). The runtime's floor (0.002 TAO) is far
above the deposit, so a partial sale cannot be below it.

## 5. What the sdk refuses, from the chain's side

Each `raw` run signs what the sdk's estimate refuses, so the chain's own answer is on record. Each
keeps its fee, which is why the sdk refuses them. Use C, which holds no TAO. Read C's numbers first:

```sh
export BT_CONVERT_TESTNET_PHRASE='the phrase of C'
BT_CONVERT_TESTNET_AMOUNT_RAO=0 go test -count=1 -run TestConvertTestnetRehearsal -v .
```

Set `LIMIT` to a value a little under `price_rao`, such as 90% of it.

| Run | `BT_CONVERT_TESTNET_AMOUNT_RAO` | `BT_CONVERT_TESTNET_LIMIT_RAO` | Expected `failure` |
|---|---|---|---|
| Under the floor | half of `min_amount_rao` | `LIMIT` | `SubtensorModule.AmountTooLow` |
| The whole stake: the fee came first | `stake_rao` | `LIMIT` | `SubtensorModule.NotEnoughStakeToWithdraw` |
| A limit over the price | `max_amount_rao` | twice `price_rao` | `Swap.PriceLimitExceeded` |
| More than the limit allows | `max_amount_rao` | `price_rao`, read just before | `SubtensorModule.SlippageTooHigh` |

```sh
BT_CONVERT_TESTNET_ACTION=raw BT_CONVERT_TESTNET_AMOUNT_RAO=AMOUNT BT_CONVERT_TESTNET_LIMIT_RAO=LIMIT go test -count=1 -run TestConvertTestnetRehearsal -v .
```

After these C still holds no TAO, since nothing was sold, and each run's `alpha fee` is on its line.
A refund too small to open an account stays with the chain, so `free` stays 0. `stake_rao` falls
by the fee each time: read it again before each run, and top C up from A if it runs low.

## 6. The remainder

Top up C if needed so it holds about 3 times `min_amount_rao`, with no TAO. Convert `min_amount_rao`,
which leaves less than the nominator minimum (0.02 TAO of value):

```sh
BT_CONVERT_TESTNET_ACTION=convert BT_CONVERT_TESTNET_AMOUNT_RAO=MIN go test -count=1 -run TestConvertTestnetRehearsal -v .
```

The estimate must show `remainder_swept_alpha_rao` and say so in its `notice`. The `after` line must
show stake 0, and the receipt's `tao_received_rao` must be close to `expected_tao_rao`.

## 7. A key that holds TAO

B holds TAO from step 4. Convert again:

```sh
export BT_CONVERT_TESTNET_PHRASE='the phrase of B'
BT_CONVERT_TESTNET_ACTION=convert BT_CONVERT_TESTNET_AMOUNT_RAO=MIN go test -count=1 -run TestConvertTestnetRehearsal -v .
```

The estimate shows `fee_in_alpha: false`, and the receipt shows `fee_tao_rao` with `fee_alpha_rao` 0.

## 8. Other wallets (for the docs, not for shipping)

The plan's open question: can a key with no TAO submit from another wallet? On a fresh key funded as
in step 2, try `btcli stake remove --network test --wallet-name ... --netuid N --include-hotkeys H --amount ...`
and the same from Talisman and Nova, and record what each does.

## 9. Report

Send back every step's output: block numbers, transaction hashes, the spec version, and anything
that differed from what is written here. A difference stops the release, and it is the thing to fix.
