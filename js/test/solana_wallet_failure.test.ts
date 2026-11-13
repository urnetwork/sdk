import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  SOLANA_WALLET_FAILURE_MESSAGE_KEYS,
  SOLANA_WALLET_FAILURE_MESSAGE_SESSION_NOT_FOUND,
  SOLANA_WALLET_FAILURE_MESSAGE_WALLET,
  SOLANA_WALLET_FAILURE_SOURCE_BROWSER,
  SOLANA_WALLET_FAILURE_USER_REJECTED,
  solanaWalletFailureFor,
} from "../src/utils/solana_wallet_failure.ts";

// The Solana wallet sign-in failures replay test/solana_wallet_failure_vectors.json,
// the vectors sdk/solana_wallet_failure_test.go replays against the Go twin,
// so ur.io reads a failure the way every app does.

const vectors = JSON.parse(readFileSync(new URL("./solana_wallet_failure_vectors.json", import.meta.url), "utf8"));

test("every vector reads the same as the Go twin", () => {
  assert.ok(vectors.cases.length >= 20, `only ${vectors.cases.length} vectors`);
  for (const c of vectors.cases) {
    assert.deepEqual(
      solanaWalletFailureFor(c.source, c.code, c.wallet_name),
      { failure: c.failure, messageKey: c.message_key, walletName: c.message_wallet_name },
      `solanaWalletFailureFor(${JSON.stringify(c.source)}, ${JSON.stringify(c.code)}, ${JSON.stringify(c.wallet_name)})`,
    );
  }
});

test("a browser wallet's numeric code may be passed as a number", () => {
  // an injected provider's error carries `code` as a number
  assert.equal(solanaWalletFailureFor(SOLANA_WALLET_FAILURE_SOURCE_BROWSER, 4001, "phantom").failure, SOLANA_WALLET_FAILURE_USER_REJECTED);
  const unknown = solanaWalletFailureFor(SOLANA_WALLET_FAILURE_SOURCE_BROWSER, undefined, null);
  assert.deepEqual(unknown, { failure: "wallet_error", messageKey: SOLANA_WALLET_FAILURE_MESSAGE_WALLET, walletName: "Solana" });
});

test("the message keys are the seven every app displays, each reachable", () => {
  assert.equal(SOLANA_WALLET_FAILURE_MESSAGE_KEYS.length, 7);
  assert.equal(new Set(SOLANA_WALLET_FAILURE_MESSAGE_KEYS).size, 7);
  const shown = new Set(vectors.cases.map((c: { message_key: string }) => c.message_key));
  for (const key of SOLANA_WALLET_FAILURE_MESSAGE_KEYS) assert.ok(shown.has(key), `no vector shows ${key}`);
  assert.ok(SOLANA_WALLET_FAILURE_MESSAGE_KEYS.includes(SOLANA_WALLET_FAILURE_MESSAGE_SESSION_NOT_FOUND));
});
