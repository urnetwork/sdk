// The drift test's model (lag.dev/DRIFTTEST.md V, I4): the TypeScript twin of
// connect's `lagdrift`, whose Go is authoritative and pins this one to 10⁻⁶
// by vectors (V9, test/lag/drift-vectors.json). The browser's fit animates
// the radar, ends a run (V10) and gives the demo its local verdict (W8); a
// real token's verdict is the server's.

export { bearingDeg, distanceKm, earthRadiusKm, fromEastNorth, toEastNorth } from "./geo";
export type { EastNorth, Point } from "./geo";
export { estimateRoundTrip, roundTripQuantile } from "./roundTrip";
export type { RoundTrip } from "./roundTrip";
export { defaultConfidence, defaultModel, earlyK, speedKmPerMs, vacuumKmPerMs } from "./model";
export type { Model, Observation } from "./model";
export { ellipse, fitPosition, minFitExtenders } from "./fit";
export type { Cov2, Ellipse, Fit } from "./fit";
export { countryContains } from "./polygon";
export type { Country } from "./polygon";
export { judge, kForConfidence, regionOutline, regionRadiusKm } from "./judge";
export type { Judgement } from "./judge";
export { physicallyPossible } from "./physical";
export {
  browserStopTolerance,
  evaluateStop,
  serverStopTolerance,
  stopCapMs,
  stopFromMs,
  stopMinExtenders,
  stopMinRoundTrips,
  stopSilenceMs,
  stopSteadyMs,
} from "./stop";
export type { Snapshot, StopReason, StopTolerance } from "./stop";
export { Borders, driftCountryWithinKm } from "./borders";
export { loadDriftWorld, readBorders } from "./world";
export type { DriftWorldData } from "./world";
export {
  driftMaxSigmaKm,
  driftMinRunExtenders,
  driftRunCounts,
  driftRunObservations,
  driftUsableRoundTrips,
  driftWindowRuns,
  localDriftVerdict,
} from "./demo";
export type {
  DriftRun,
  DriftRunExtender,
  DriftVerdict,
  DriftVerdictReason,
  LocalDriftVerdict,
  LocalDriftVerdictOptions,
} from "./demo";
