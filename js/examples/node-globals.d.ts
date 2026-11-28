// The few Node globals the examples use, for this repository's type check
// only (tsconfig.json here): the sdk has no @types/node. A project of your
// own that has @types/node leaves this file out.
declare const process: {
  env: Record<string, string | undefined>;
  exit(code?: number): never;
  on(event: "SIGINT" | "SIGTERM", listener: () => void): void;
};
