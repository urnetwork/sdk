// A MeasureContext on this machine's network: the api client, the guest kept
// between runs, and sessions that reach the extenders as lag.dev does.

import { join } from "node:path";
import { DEFAULT_API_BASE_URL } from "../../src/api_client";
import { createLagApiClient } from "../../src/lag/api";
import { openLagSession } from "../../src/lag/client";
import { LagGuestSession, memoryLagStorage } from "../../src/lag/session";
import { defaultConfigDir, fileLagStorage, guestFileName } from "./guest_store";
import type { MeasureContext } from "./measure";
import { createNet, type NetOptions } from "./net";

export interface ContextOptions extends NetOptions {
  /** the api (default https://api.bringyour.com) */
  apiURL?: string;
  /** keep the guest between runs (default true) */
  save?: boolean;
  /** where it's kept (default defaultConfigDir()) */
  configDir?: string;
  onProgress?: (line: string) => void;
}

export interface LocalContext {
  context: MeasureContext;
  close(): Promise<void>;
}

export function createContext(options: ContextOptions = {}): LocalContext {
  const apiURL = options.apiURL ?? DEFAULT_API_BASE_URL;
  const net = createNet(options);
  const configDir = options.configDir ?? defaultConfigDir();
  const storage =
    options.save === false
      ? memoryLagStorage()
      : fileLagStorage(join(configDir, guestFileName(apiURL, options.connectTo)), configDir);
  const guest = new LagGuestSession({
    storage,
    api: createLagApiClient({ baseURL: apiURL, fetch: net.apiFetch, timeoutMs: 20_000 }),
  });
  const api = createLagApiClient({
    baseURL: apiURL,
    fetch: net.apiFetch,
    token: () => guest.token(),
    timeoutMs: 20_000,
  });
  const context: MeasureContext = {
    api,
    guest,
    apiOrigin: new URL(apiURL).origin,
    meter: net.meter,
    onProgress: options.onProgress,
    openSession: ({ host, ticket, onEvent }) =>
      openLagSession({ host, ticket, onEvent, fetch: net.measureFetch, WebSocket: net.WebSocket }),
  };
  return { context, close: () => net.close() };
}
