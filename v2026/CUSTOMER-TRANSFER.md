# Customer transfer request identity

Create one `WalletCircleTransferOutArgs` per user intent with
`NewWalletCircleTransferOutArgs(address, amount, terms)`. Persist its complete
JSON, including `request_id`, before the first API call. Reuse that saved JSON
for transport retries and after application restart. Generate a new id only
for a distinct intended transfer, including a deliberate second transfer of
the same amount to the same address.

The JavaScript API requires `request_id` in its typed request. Generate it once
with the platform UUID facility and persist the complete request before calling
`walletCircleTransferOut`. The generated C++ request has the same field; C ABI
callers include it in their request JSON. Use an exact integer amount: this API
represents one dollar as `1000000000`, and USDC amounts must be positive multiples
of `1000`. The JavaScript client refuses values beyond `Number.MAX_SAFE_INTEGER`
before token lookup or HTTP; it does not support the full int64 monetary range.

The server retains the original wallet, destination, amount, provider key, and
request bytes before submission. An unknown response is retried only with the
same request id. A known challenge is observed with GET and never causes a new
transfer challenge. `challenge_status=COMPLETE` describes user confirmation of
the challenge; it does not prove payment settlement. Conflicting terms under
an existing id are refused. A transport error or cancellation must not cause
the application to discard the saved intent and generate another id.

Older callers without `request_id` receive an explicit error before a transfer
challenge is submitted. Deploy the server custody migration and updated client
request handling together. These SDK changes do not migrate external web or
mobile applications automatically; those applications must persist the id
through their own lifecycle. Existing pre-upgrade unknown challenges cannot
be reconstructed from a new request id and need their original provider
evidence before another intent is created.
