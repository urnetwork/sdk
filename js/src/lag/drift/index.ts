// The drift test's model (lag.dev/DRIFTTEST.md V, I4): the TypeScript twin of
// connect's `lagdrift`, whose Go is authoritative. Names are lagdrift's in
// camelCase, and every formula is evaluated in its order, pinned to it to
// 10⁻⁶ by vectors (V9, test/lag/drift-vectors.json). The browser's fit
// animates the radar, ends a run (V10) and gives the demo its local verdict
// (W8); a real token's verdict is the server's.

export {
  azimuthalKm,
  bearingDeg,
  distanceKm,
  earthRadiusKm,
  fromAzimuthalKm,
  moveKm,
  pointValid,
} from "./geo";
export type { EastNorth, Point } from "./geo";
export { estimateRoundTrip, minRoundTripSamples, roundTripQuantile } from "./roundTrip";
export type { RoundTrip } from "./roundTrip";
export {
  defaultConfidence,
  defaultKappa,
  defaultModel,
  defaultSigma0Ms,
  derivedPlaceErrorKm,
  earlyK,
  heldPlaceErrorKm,
  maxPlaceErrorKm,
  minPlaceErrorKm,
  msPerKm,
  speedKmPerMs,
  vacuumKmPerMs,
} from "./model";
export type { Model, Observation } from "./model";
export { covEllipse, fitPosition, huberC, maxStepKm, minFitExtenders } from "./fit";
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
  stopEarliestMs,
  stopExtenders,
  stopHolds,
  stopInterruptedMs,
  stopRoundTrips,
  stopWindowMs,
} from "./stop";
export type { Snapshot, StopReason, StopTolerance } from "./stop";
export { decide, defaultMaxSigmaKm } from "./verdict";
export type { Decision, DriftVerdict, DriftVerdictReason } from "./verdict";
export { Borders, driftCountryWithinKm } from "./borders";
export { loadDriftWorld, readWorld, worldFormat } from "./world";
export type { DriftWorldData } from "./world";
export { bordersFormatVersion, bordersMagic, readBorders } from "./bordersFormat";
export {
  driftMinRunExtenders,
  driftRunCounts,
  driftRunObservations,
  driftUsableRoundTrips,
  driftWindowRuns,
  localDriftVerdict,
} from "./demo";
export type { DriftRun, DriftRunExtender, LocalDriftVerdict, LocalDriftVerdictOptions } from "./demo";
