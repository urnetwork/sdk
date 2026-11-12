/**
 * Which account holds the plan (support inbox 6546): the account name ur.io
 * shows with the plan and in the purchase confirmation.
 *
 * This is the TypeScript twin of `sdk/plan_account.go`: `planAccountName` is
 * Go's `PlanAccountName`. The Go version is what the apps bind; this is what
 * the web imports. test/plan_account_vectors.json holds the vectors both test
 * suites replay.
 */

// A legacy guest network's name: "g" and its network id, with or without the
// id's dashes. Nobody signs in with it.
const planAccountGuestNamePattern = /^g[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$/;

/**
 * The account name to show with the plan: the signed-in network's own name,
 * trimmed. "" for a guest network (known from `guest`, or from its name) and
 * when no name is known: then the plan shows alone. It never names another
 * network.
 */
export function planAccountName(networkName: string | null | undefined, guest: boolean): string {
  const name = (networkName ?? "").trim();
  if (guest || planAccountGuestNamePattern.test(name)) {
    return "";
  }
  return name;
}
