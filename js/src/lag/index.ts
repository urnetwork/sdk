// The lag.dev browser SDK (lag.dev/PLAN.md S), as `@urnetwork/sdk/lag`:
// plain TypeScript that never loads the wasm, for a page that needs only
// fetch, WebSocket and WebCrypto.
//
//   import { openLagSession, countryColorHex, extenderColorHex } from "@urnetwork/sdk/lag";
//
// Colors, names, the protobuf reader and the feed client are S1-S3; the guest
// session, the api calls, the lag client, its keystream and its statistics
// are S4-S7.

export { COUNTRY_COLOR_HEXES } from "./country_colors.generated";
export {
  UNKNOWN_COUNTRY_COLOR_HEX,
  countryColorHex,
  extenderColorHex,
  extenderHue,
  extenderHueColorHex,
} from "./colors";
export {
  DEFAULT_EXTENDER_MEASUREMENT_ZONE,
  canonicalIp,
  extenderIdFromBytes,
  extenderMeasurementHost,
  isExtenderId,
  parseExtenderMeasurementHost,
} from "./names";
export {
  ProtobufError,
  decodeExtenderFeedFrame,
  decodeExtenderRecord,
  decodeExtenderRecordBody,
  decodeExtenderRevocation,
  decodeExtenderRevocationBody,
  decodeLagTicket,
  decodeLagTicketBody,
  encodeExtenderFeedRequest,
} from "./protobuf";
export type {
  ExtenderAddress,
  ExtenderFeedFrame,
  ExtenderFeedRequest,
  ExtenderRecord,
  ExtenderRecordBody,
  ExtenderRevocation,
  ExtenderRevocationBody,
  LagTicketBody,
  SignedLagTicket,
} from "./protobuf";
export {
  DEFAULT_EXTENDER_FEED_SAMPLE_COUNT,
  DEFAULT_EXTENDER_RECORD_EXPIRE_SKEW_MS,
  EXTENDER_FEED_MAX_FRAME_BYTES,
  EXTENDER_RECORD_SIGNATURE_DOMAIN,
  EXTENDER_REVOCATION_SIGNATURE_DOMAIN,
  LAG_TICKET_SIGNATURE_DOMAIN,
  ExtenderDirectory,
  ExtenderFeedClient,
  ExtenderFeedError,
  ExtenderRootKeys,
  verifyEd25519,
} from "./feed";
export type {
  ExtenderApplyResult,
  ExtenderDirectoryEntry,
  ExtenderDirectoryOptions,
  ExtenderFeedClientOptions,
  ExtenderFeedEvent,
  ExtenderFeedSampleOptions,
  ExtenderFeedTransportOptions,
  ExtenderRefusal,
} from "./feed";

export * from "./api";
export * from "./client";
export * from "./keystream";
export * from "./session";
export * from "./stats";
