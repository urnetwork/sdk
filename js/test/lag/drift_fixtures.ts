// Synthetic geometries for the drift model's tests (lag.dev/DRIFTTEST.md T2):
// visitors at known places, extenders at known places, round trips from the
// model's own form with seeded noise, and square or shaped countries.

import type { Country, DriftRun, Observation, Point } from "../../src/lag/drift/index.ts";
import { distanceKm, fromAzimuthalKm, speedKmPerMs } from "../../src/lag/drift/index.ts";

export const K = 2 / speedKmPerMs;

// a seeded generator, so a failure reproduces
export function random(seed: number): () => number {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function gaussian(rng: () => number): number {
  const u = Math.max(1e-12, rng());
  return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * rng());
}

export const CHICAGO: Point = { lat: 41.8781, lon: -87.6298 };
export const MADRID: Point = { lat: 40.4168, lon: -3.7038 };
export const FRANKFURT: Point = { lat: 50.1109, lon: 8.6821 };

// 12 extenders around Chicago: near, mid and far (E2)
export const CHICAGO_EXTENDERS: Point[] = [
  { lat: 43.0389, lon: -87.9065 },
  { lat: 39.7684, lon: -86.1581 },
  { lat: 42.3314, lon: -83.0458 },
  { lat: 38.627, lon: -90.1994 },
  { lat: 43.6532, lon: -79.3832 },
  { lat: 33.749, lon: -84.388 },
  { lat: 32.7767, lon: -96.797 },
  { lat: 39.7392, lon: -104.9903 },
  { lat: 47.6062, lon: -122.3321 },
  { lat: 34.0522, lon: -118.2437 },
  { lat: 51.5074, lon: -0.1278 },
  { lat: -23.5505, lon: -46.6333 },
];

// 12 extenders around western Europe
export const EUROPE_EXTENDERS: Point[] = [
  { lat: 41.3874, lon: 2.1686 },
  { lat: 39.4699, lon: -0.3763 },
  { lat: 38.7223, lon: -9.1393 },
  { lat: 43.263, lon: -2.935 },
  { lat: 48.8566, lon: 2.3522 },
  { lat: 45.4642, lon: 9.19 },
  { lat: 51.5074, lon: -0.1278 },
  { lat: 33.5731, lon: -7.5898 },
  { lat: 59.3293, lon: 18.0686 },
  { lat: 41.0082, lon: 28.9784 },
  { lat: 52.3676, lon: 4.9041 },
  { lat: 52.2297, lon: 21.0122 },
];

/**
 * a square country `halfKm` from its center each way, as one polygon, square
 * in the azimuthal equidistant projection centered on it (vertices every 10 km)
 */
export function squareCountry(code: string, center: Point, halfKm: number): Country {
  const corners: [number, number][] = [
    [-halfKm, -halfKm],
    [halfKm, -halfKm],
    [halfKm, halfKm],
    [-halfKm, halfKm],
  ];
  const ring: Point[] = [];
  for (let i = 0; i < 4; i++) {
    const [ae, an] = corners[i];
    const [be, bn] = corners[(i + 1) % 4];
    const steps = Math.max(1, Math.ceil((2 * halfKm) / 10));
    for (let s = 0; s < steps; s++) {
      const f = s / steps;
      ring.push(fromAzimuthalKm(center, ae + (be - ae) * f, an + (bn - an) * f));
    }
  }
  return { code, polygons: [[ring]] };
}

/** a ring's edges cut into pieces of at most `stepKm`, so straight-in-degrees and great-circle agree */
export function densify(ring: Point[], stepKm: number): Point[] {
  const out: Point[] = [];
  for (let i = 0; i < ring.length; i++) {
    const a = ring[i];
    const b = ring[(i + 1) % ring.length];
    const steps = Math.max(1, Math.ceil(distanceKm(a, b) / stepKm));
    for (let s = 0; s < steps; s++) {
      const f = s / steps;
      out.push({ lat: a.lat + (b.lat - a.lat) * f, lon: a.lon + (b.lon - a.lon) * f });
    }
  }
  return out;
}

/** a ring of places at the given east and north km from a center */
export function ringAround(center: Point, offsetsKm: [number, number][]): Point[] {
  return offsetsKm.map(([e, n]) => fromAzimuthalKm(center, e, n));
}

export interface SyntheticOptions {
  truth: Point;
  extenders: Point[];
  /** the last-mile offset, ms */
  lastMileMs: number;
  /** the route stretch of each path, a share of its distance; default none */
  stretch?: number[];
  /** round trips per extender */
  count?: number;
  /** mean queueing added to each ping, ms (exponential); default none */
  queueMs?: number;
  seed?: number;
  /** ids, default e0, e1, … */
  ids?: string[];
}

/** each extender's round trips, in the order sent */
export function syntheticSamples(options: SyntheticOptions): number[][] {
  const rng = random(options.seed ?? 1);
  const count = options.count ?? 16;
  return options.extenders.map((at, i) => {
    const base = options.lastMileMs + K * distanceKm(options.truth, at) * (1 + (options.stretch?.[i] ?? 0));
    return Array.from({ length: count }, () => base + (options.queueMs ? -Math.log(Math.max(1e-9, rng())) * options.queueMs : 0));
  });
}

/** one run's observations with exact round trips (no queueing) and a given error */
export function exactObservations(options: SyntheticOptions & { run?: number; sigmaMs?: number; placeErrorKm?: number }): Observation[] {
  return options.extenders.map((at, i) => ({
    extenderId: options.ids?.[i] ?? `e${i}`,
    at,
    placeErrorKm: options.placeErrorKm ?? 25,
    run: options.run ?? 1,
    roundTrip: {
      ms: options.lastMileMs + K * distanceKm(options.truth, at) * (1 + (options.stretch?.[i] ?? 0)),
      sigmaMs: options.sigmaMs ?? 0.5,
      count: 16,
      // the lowest sample unknown: the physical check uses ms
      minMs: 0,
    },
  }));
}

/** a run as the frame holds it */
export function syntheticRun(run: number, options: SyntheticOptions): DriftRun {
  const samples = syntheticSamples(options);
  return {
    run,
    extenders: options.extenders.map((at, i) => ({
      extenderId: options.ids?.[i] ?? `e${i}`,
      at,
      placeErrorKm: 25,
      roundTripsMs: samples[i],
    })),
  };
}
