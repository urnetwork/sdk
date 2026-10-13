// Where the CLI keeps its lag.dev guest between runs (lag.dev/PLAN.md A1, S4).
//
// A page keeps its guest in localStorage so a reload reuses it. The CLI
// keeps it in a file for the same reason: the api creates at most 10 guests
// an hour per network (/24 or /48), so a run that made a new guest each time
// would soon be refused. One file per api, readable only by the user. The
// guest is a one-time network that expires within 24 hours and can only
// measure: it holds no address and nothing about the person running it.

import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import type { LagStorage } from "../../src/lag/session";

/** The CLI's own directory: $XDG_CONFIG_HOME/lagdev, else ~/.config/lagdev (%APPDATA%\lagdev on Windows). */
export function defaultConfigDir(env: NodeJS.ProcessEnv = process.env): string {
  if (env.XDG_CONFIG_HOME) {
    return join(env.XDG_CONFIG_HOME, "lagdev");
  }
  if (process.platform === "win32" && env.APPDATA) {
    return join(env.APPDATA, "lagdev");
  }
  return join(homedir(), ".config", "lagdev");
}

/**
 * A file name for one api, e.g. "api.bringyour.com" or, for a test stack
 * reached through another address, "api.bringyour.com@127.0.0.1-47443":
 * a guest from one is no good at the other.
 */
export function guestFileName(apiURL: string, connectTo?: { host: string; port: number }): string {
  const url = new URL(apiURL);
  const parts = [url.host];
  if (connectTo) {
    parts.push(`${connectTo.host}-${connectTo.port}`);
  }
  return `guest-${parts.join("@").replace(/[^A-Za-z0-9.@-]/g, "_")}.json`;
}

/**
 * A LagStorage over one JSON file of key → value strings. Writes go through a
 * temporary file and a rename, so a crash never leaves half a guest.
 */
export function fileLagStorage(path: string, dir: string): LagStorage {
  const read = async (): Promise<Record<string, string>> => {
    try {
      const parsed: unknown = JSON.parse(await readFile(path, "utf8"));
      return typeof parsed === "object" && parsed !== null ? (parsed as Record<string, string>) : {};
    } catch {
      return {};
    }
  };
  const write = async (values: Record<string, string>): Promise<void> => {
    if (Object.keys(values).length === 0) {
      await rm(path, { force: true });
      return;
    }
    await mkdir(dir, { recursive: true, mode: 0o700 });
    const temporary = `${path}.${process.pid}.tmp`;
    await writeFile(temporary, JSON.stringify(values), { mode: 0o600 });
    await rename(temporary, path);
  };
  return {
    get: async (key) => (await read())[key] ?? null,
    set: async (key, value) => {
      const values = await read();
      values[key] = value;
      await write(values);
    },
    remove: async (key) => {
      const values = await read();
      delete values[key];
      await write(values);
    },
  };
}
