// The URnetwork api client alone — no wasm, no DOM — for a MV3 service
// worker, a web worker or any fetch-only runtime:
//
//   import { createURNetworkApiClient } from "@urnetwork/sdk/client";
//
// The package root ("@urnetwork/sdk") re-exports all of this too.

export * from "./api_client";
export {
  URNetworkApiClient,
  createURNetworkApiClient,
  OPENAPI_SPEC_VERSION,
  OPENAPI_SPEC_SHA256,
} from "./generated/client";
// the spec's schema types, as a namespace so a spec name (e.g.
// OpenAPI.AuthLoginArgs) never collides with a hand-written or Go-reflected
// type of the same name
export type * as OpenAPI from "./generated/openapi";
export { URNetworkAPI } from "./api";
export type { URNetworkAPIConfig } from "./api";
