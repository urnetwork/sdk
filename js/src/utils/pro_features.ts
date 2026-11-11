/**
 * The "Pro features in use" notice: a network without Pro whose hosted
 * proxies hold SOCKS or WireGuard credentials, the two proxy features pro.yml
 * puts in Pro.
 *
 * This is the TypeScript twin of `sdk/pro_features_status.go`: the functions
 * have the Go names in camelCase, and the constants the Go names in
 * UPPER_SNAKE_CASE, as this package names its constants. The Go version is
 * what the apps bind; this is what the web imports. The two must stay in
 * step, and test/pro_features_vectors.json holds the vectors both test suites
 * replay.
 *
 * While the server's enforce_features switch is off, every plan uses SOCKS and
 * WireGuard. Once it is on, the proxy refuses them to a network without Pro: a
 * SOCKS sign-in fails, a WireGuard peer is dropped, and a new proxy is issued
 * neither. HTTP and HTTPS keep working.
 *
 * The server decides everything the notice says (GET /network/proxies): the
 * list's `plan` (whether the network holds Pro now, read live, and whether
 * enforce_features is on) and each proxy's `pro_features` (the Pro-only
 * features it holds credentials for). A server before the plan reports none,
 * and the notice stays off.
 *
 * The prompt (proFeaturesStatusFor(...).prompt) is what a surface shows where
 * it lists the network's proxies; ProFeaturesNoticeTracker decides the notice
 * on the main screen, with the client limit notice's rules.
 */

import type { NetworkClientsResult } from "../generated/openapi";

/** The pro.yml feature names the server lists in a proxy's `pro_features`. */
export const PRO_FEATURE_SOCKS_PROXY = "socks_proxy";
export const PRO_FEATURE_WIREGUARD_PROXY = "wireguard_proxy";

/** Nothing to say: the network holds Pro, holds no SOCKS or WireGuard proxy, or the server did not report its plan. */
export const PRO_FEATURES_PROMPT_NONE = "";
/** Enforcement is off: SOCKS and WireGuard proxies are part of Pro, and an upgrade keeps them working. */
export const PRO_FEATURES_PROMPT_UPGRADE = "upgrade";
/** Enforcement is on: SOCKS and WireGuard stopped working on the network's proxies, and an upgrade turns them back on. */
export const PRO_FEATURES_PROMPT_STOPPED = "stopped";

export type ProFeaturesPrompt =
  | typeof PRO_FEATURES_PROMPT_NONE
  | typeof PRO_FEATURES_PROMPT_UPGRADE
  | typeof PRO_FEATURES_PROMPT_STOPPED;

/** Leave the notice as it is. */
export const PRO_FEATURES_NOTICE_NONE = "";
/** Show the notice and alert the user. */
export const PRO_FEATURES_NOTICE_POST = "post";
/** Show the notice, or replace the one showing, without alerting. */
export const PRO_FEATURES_NOTICE_UPDATE = "update";
/** Withdraw the notice. */
export const PRO_FEATURES_NOTICE_REMOVE = "remove";

export type ProFeaturesNoticeAction =
  | typeof PRO_FEATURES_NOTICE_NONE
  | typeof PRO_FEATURES_NOTICE_POST
  | typeof PRO_FEATURES_NOTICE_UPDATE
  | typeof PRO_FEATURES_NOTICE_REMOVE;

/** One readout of the network's Pro features in use (Go `ProFeaturesStatus`). */
export interface ProFeaturesStatus {
  prompt: ProFeaturesPrompt;
  /** The proxies that hold a SOCKS url, of the features the free plan does not include. */
  socksProxyCount: number;
  /** The proxies that hold a WireGuard config. A proxy with both counts in both. */
  wireguardProxyCount: number;
  /** The server reported the network's plan with its proxies. */
  planKnown: boolean;
  /** The network holds Pro now (planKnown only). */
  pro: boolean;
  /** The server refuses SOCKS and WireGuard to a network without Pro (planKnown only). */
  proFeaturesEnforced: boolean;
}

/** The proxies list as it arrives, loosely: a surface may hand over any part of it. */
export type ProFeaturesProxiesList =
  | Pick<NetworkClientsResult, "plan"> & {
      clients?: Array<{ pro_features?: string[] | null } | null> | null;
    }
  | null
  | undefined;

/**
 * Reads the network's proxies list (GET /network/proxies) into the notice's
 * status. A missing list, or one without the plan, has no prompt.
 */
export function proFeaturesStatusFor(result: ProFeaturesProxiesList): ProFeaturesStatus {
  const status: ProFeaturesStatus = {
    prompt: PRO_FEATURES_PROMPT_NONE,
    socksProxyCount: 0,
    wireguardProxyCount: 0,
    planKnown: false,
    pro: false,
    proFeaturesEnforced: false,
  };
  if (!result) return status;
  for (const client of Array.isArray(result.clients) ? result.clients : []) {
    const features = Array.isArray(client?.pro_features) ? client.pro_features : [];
    if (features.includes(PRO_FEATURE_SOCKS_PROXY)) status.socksProxyCount += 1;
    if (features.includes(PRO_FEATURE_WIREGUARD_PROXY)) status.wireguardProxyCount += 1;
  }
  const plan = result.plan;
  if (!plan) return status;
  status.planKnown = true;
  status.pro = plan.pro === true;
  status.proFeaturesEnforced = plan.pro_features_enforced === true;
  status.prompt = proFeaturesPromptFor(status);
  return status;
}

function proFeaturesPromptFor(status: ProFeaturesStatus): ProFeaturesPrompt {
  if (!status.planKnown || status.pro) return PRO_FEATURES_PROMPT_NONE;
  if (status.socksProxyCount === 0 && status.wireguardProxyCount === 0) return PRO_FEATURES_PROMPT_NONE;
  return status.proFeaturesEnforced ? PRO_FEATURES_PROMPT_STOPPED : PRO_FEATURES_PROMPT_UPGRADE;
}

/**
 * The notice a surface shows on its main screen for the network's Pro
 * features in use (Go `ProFeaturesNoticeTracker`), with the client limit
 * notice's rules and a sign-in in place of a connection request:
 *   - the notice shows while the status has a prompt;
 *   - it alerts once per sign-in: a notice that returns after it went shows
 *     again without alerting, and a prompt change (enforcement turning on)
 *     replaces it in place;
 *   - it goes when the prompt ends (an upgrade, or no SOCKS or WireGuard proxy
 *     left) and at reset (sign out);
 *   - dismiss, the user closing it, withdraws it until its prompt changes or
 *     it ends and returns, and reset clears that.
 *
 * The list of proxies shows the status's prompt whatever the tracker says.
 */
export class ProFeaturesNoticeTracker {
  private shown = false;
  private shownPrompt: ProFeaturesPrompt = PRO_FEATURES_PROMPT_NONE;
  private alerted = false;
  private dismissedPrompt: ProFeaturesPrompt = PRO_FEATURES_PROMPT_NONE;

  /** Takes the network's current status and returns what to do with the notice. */
  update(status: Pick<ProFeaturesStatus, "prompt"> | null | undefined): ProFeaturesNoticeAction {
    const prompt = status?.prompt ?? PRO_FEATURES_PROMPT_NONE;
    if (prompt === PRO_FEATURES_PROMPT_NONE) {
      // a prompt that returns later shows again, quietly
      this.dismissedPrompt = PRO_FEATURES_PROMPT_NONE;
      return this.withdraw();
    }
    if (prompt === this.dismissedPrompt) return this.withdraw();
    // a new prompt ends the dismissal of the old one
    this.dismissedPrompt = PRO_FEATURES_PROMPT_NONE;
    if (this.shown && this.shownPrompt === prompt) return PRO_FEATURES_NOTICE_NONE;
    this.shown = true;
    this.shownPrompt = prompt;
    if (this.alerted) return PRO_FEATURES_NOTICE_UPDATE;
    this.alerted = true;
    return PRO_FEATURES_NOTICE_POST;
  }

  /** The user closing the notice: it stays withdrawn while its prompt stays the same. */
  dismiss(): ProFeaturesNoticeAction {
    if (!this.shown) return PRO_FEATURES_NOTICE_NONE;
    this.dismissedPrompt = this.shownPrompt;
    return this.withdraw();
  }

  /** Ends the sign-in, for a sign out or an account switch. */
  reset(): ProFeaturesNoticeAction {
    this.alerted = false;
    this.dismissedPrompt = PRO_FEATURES_PROMPT_NONE;
    return this.withdraw();
  }

  /** The prompt of the notice showing, PRO_FEATURES_PROMPT_NONE when none does. */
  get shownPromptNow(): ProFeaturesPrompt {
    return this.shown ? this.shownPrompt : PRO_FEATURES_PROMPT_NONE;
  }

  private withdraw(): ProFeaturesNoticeAction {
    if (!this.shown) return PRO_FEATURES_NOTICE_NONE;
    this.shown = false;
    this.shownPrompt = PRO_FEATURES_PROMPT_NONE;
    return PRO_FEATURES_NOTICE_REMOVE;
  }
}
