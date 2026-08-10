/**
 * Solana Pay, shared by every platform.
 *
 * This is the TypeScript twin of `sdk/solana_pay.go`. The two must stay in step --
 * the Go version is what the mobile apps bind, this is what the web app imports.
 * The rules they encode were each learned from a shipped bug:
 *
 *   - The web app generated the reference as a hex uuid. Solana Pay requires the
 *     reference to be a base58 32-byte public key: the wallet attaches it to the
 *     transaction as a read-only account, and the webhook matches the on-chain
 *     account list against `solana_payment_intent.payment_reference`. A hex string
 *     is not a valid pubkey and never appears in that list, so the customer paid
 *     and the payment could never be matched back to their account.
 *   - Android built the url with a hardcoded amount and message, so it could not
 *     sell the monthly plan and its price no longer had to agree with the server's.
 *
 * The rule: the client never names its own price. Register the intent with the
 * plan, take `amount_usd` from the response, and pass THAT to buildSolanaPaymentUrl.
 */

/** A Solana public key is 32 bytes; that is what the `reference` must be. */
export const SOLANA_PAY_REFERENCE_BYTES = 32;

// The Bitcoin/Solana alphabet: no 0, O, I or l.
const B58_ALPHABET =
  "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";

/**
 * Encode bytes as base58.
 *
 * Leading zero bytes become leading '1' characters -- base58 has no distinct zero
 * digit, so the count of leading zeros is carried separately. This matches
 * `base58Encode` in sdk/base58.go exactly.
 */
export function encodeBase58(bytes: Uint8Array): string {
  if (bytes.length === 0) return "";

  // Big-endian base-256 to base-58, by repeated division.
  const digits: number[] = [0];
  for (const byte of bytes) {
    let carry = byte;
    for (let i = 0; i < digits.length; i++) {
      carry += digits[i] << 8;
      digits[i] = carry % 58;
      carry = (carry / 58) | 0;
    }
    while (carry > 0) {
      digits.push(carry % 58);
      carry = (carry / 58) | 0;
    }
  }

  // digits is little-endian, so the most significant digit is last. A most
  // significant zero is a leading zero in the output and must not be emitted --
  // otherwise an all-zero input yields one character too many.
  while (digits.length > 0 && digits[digits.length - 1] === 0) digits.pop();

  let out = "";
  for (let i = 0; i < bytes.length && bytes[i] === 0; i++) out += B58_ALPHABET[0];
  for (let i = digits.length - 1; i >= 0; i--) out += B58_ALPHABET[digits[i]];
  return out;
}

/**
 * Decode base58 to bytes, or null if any character is outside the alphabet.
 *
 * The Go twin returns an empty slice rather than an error; null here is the same
 * contract in a shape TypeScript callers can check.
 */
export function decodeBase58(s: string): Uint8Array | null {
  if (s.length === 0) return new Uint8Array(0);

  const bytes: number[] = [0];
  for (const ch of s) {
    const value = B58_ALPHABET.indexOf(ch);
    if (value < 0) return null;
    let carry = value;
    for (let i = 0; i < bytes.length; i++) {
      carry += bytes[i] * 58;
      bytes[i] = carry & 0xff;
      carry >>= 8;
    }
    while (carry > 0) {
      bytes.push(carry & 0xff);
      carry >>= 8;
    }
  }

  // Same rule in reverse: trim the most significant zero bytes so an all-'1'
  // string decodes to exactly its leading zeros and nothing more.
  while (bytes.length > 0 && bytes[bytes.length - 1] === 0) bytes.pop();

  let leadingZeros = 0;
  for (let i = 0; i < s.length && s[i] === B58_ALPHABET[0]; i++) leadingZeros++;

  const out = new Uint8Array(leadingZeros + bytes.length);
  for (let i = 0; i < bytes.length; i++) out[leadingZeros + i] = bytes[bytes.length - 1 - i];
  return out;
}

/**
 * A fresh Solana Pay reference: 32 cryptographically random bytes, base58 encoded.
 *
 * The value is an identifier, not key material -- nothing signs with it. It has to
 * be unpredictable only so two customers paying at the same moment cannot collide
 * on one intent row.
 */
export function createPaymentReference(): string {
  const bytes = new Uint8Array(SOLANA_PAY_REFERENCE_BYTES);
  crypto.getRandomValues(bytes);
  return encodeBase58(bytes);
}

/**
 * Whether `s` is a well-formed reference: base58 decoding to exactly 32 bytes.
 *
 * Worth checking before opening a wallet, because the failure it catches is
 * silent -- a malformed reference does not error, the wallet drops it, the payment
 * goes through, and the webhook has nothing to match it against.
 */
export function isValidPaymentReference(s: string): boolean {
  if (!s) return false;
  const decoded = decodeBase58(s);
  return decoded !== null && decoded.length === SOLANA_PAY_REFERENCE_BYTES;
}

/** A Solana address has the same shape as a reference -- both are public keys. */
export function isBase58Address(s: string): boolean {
  if (!s) return false;
  const decoded = decodeBase58(s);
  return decoded !== null && decoded.length === SOLANA_PAY_REFERENCE_BYTES;
}

export interface SolanaPaymentUrlArgs {
  /** The merchant address, base58. Never hardcode it -- it has rotated before. */
  recipient: string;
  /** The price the SERVER quoted (`amount_usd`). Never a client-side constant. */
  amountUsd: number;
  /** The token mint address, base58 (USDC on mainnet). */
  splTokenMint: string;
  /** From createPaymentReference, already registered with the payment intent. */
  reference: string;
  /** The merchant name the wallet shows. */
  label?: string;
  /** The human description of the purchase the wallet shows. */
  message?: string;
}

/**
 * Build the `solana:` deep link for a payment.
 *
 * Throws rather than returning something unusable: every field here has a failure
 * mode that is invisible at the call site. A wrong reference format is dropped by
 * the wallet, a zero amount is a free year (the webhook's check is
 * `amount >= price - tolerance`), and a malformed address sends money nowhere
 * recoverable.
 */
export function buildSolanaPaymentUrl(args: SolanaPaymentUrlArgs): string {
  if (!args) throw new Error("solana pay: no arguments");
  if (!isBase58Address(args.recipient)) {
    throw new Error("solana pay: recipient is not a base58 address");
  }
  if (!isBase58Address(args.splTokenMint)) {
    throw new Error("solana pay: spl token mint is not a base58 address");
  }
  if (!isValidPaymentReference(args.reference)) {
    throw new Error(
      `solana pay: reference must be base58 that decodes to ${SOLANA_PAY_REFERENCE_BYTES} bytes`,
    );
  }
  if (!Number.isFinite(args.amountUsd) || args.amountUsd <= 0) {
    throw new Error(`solana pay: amount must be positive, got ${args.amountUsd}`);
  }

  const params = new URLSearchParams({
    // Plain decimal. Wallets do not parse scientific notation, which is what
    // String(0.000001) would produce.
    amount: formatAmount(args.amountUsd),
    "spl-token": args.splTokenMint,
    reference: args.reference,
  });
  if (args.label) params.set("label", args.label);
  if (args.message) params.set("message", args.message);

  return `solana:${args.recipient}?${params.toString()}`;
}

/**
 * Decimal notation always, never exponential -- wallets do not parse "1e-06".
 *
 * `toFixed` is not enough: it returns exponential again at 1e21 and above, and
 * caps at 20 fraction digits. This expands the exponent by hand, which is what
 * Go's strconv.FormatFloat(f, 'f', -1, 64) does for the twin implementation.
 */
function formatAmount(n: number): string {
  const s = String(n);
  if (!/[eE]/.test(s)) return s;

  const [mantissa, expPart] = s.split(/[eE]/);
  const exp = parseInt(expPart, 10);
  const negative = mantissa.startsWith("-");
  const unsigned = negative ? mantissa.slice(1) : mantissa;
  const dot = unsigned.indexOf(".");
  const digits = unsigned.replace(".", "");
  const pointPos = (dot < 0 ? unsigned.length : dot) + exp;

  let out: string;
  if (pointPos <= 0) {
    out = "0." + "0".repeat(-pointPos) + digits;
  } else if (pointPos >= digits.length) {
    out = digits + "0".repeat(pointPos - digits.length);
  } else {
    out = digits.slice(0, pointPos) + "." + digits.slice(pointPos);
  }
  return (negative ? "-" : "") + out;
}
