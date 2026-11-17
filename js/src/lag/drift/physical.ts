// The physical check (lag.dev/DRIFTTEST.md V5), as connect's lagdrift makes it
// (physical.go). Nothing travels faster than light in a vacuum, so the
// visitor is within
//
//   Rᵢ = (yᵢ,min / 2) × 299.79 km + s_ext,i
//
// of each extender, where yᵢ,min is the lowest round trip to it in any run.
// The IP country is physically possible when, with some single extender's
// disc dropped, it meets the area where all the other discs overlap: one
// misplaced extender or one broken path can't make a measurement contradict
// itself.
//
// For each i let Iᵢ be the intersection of every disc but i's. The country C
// meets some Iᵢ exactly when (a) a point of C's boundary lies in Iᵢ, or (b) Iᵢ
// lies inside C:
//
//   - (a) C's boundary is sampled at its vertices and at the points its edges
//     are split at, every 0.25° as the region test splits them, so every
//     point of it is within 20 km of a sample. A sample counts when it is
//     within Rⱼ + 20 km of every extender but at most one, so the check errs
//     toward possible, the side that keeps the region's verdict.
//   - (b) A nonempty Iᵢ has a point in it among the extenders' places, one
//     point on each disc's circle, and the points where two circles cross. A
//     candidate counts when it is within 1 m of every disc but at most one and
//     C contains it.
//
// The check never rejects on its own (V5): only the region does. A missing
// country, or fewer than two extenders, is always possible.

import {
  add,
  cross,
  dot,
  earthRadiusKm,
  norm,
  scale,
  unitVector,
  vectorPoint,
  type Point,
  type Vector3,
} from "./geo";
import { splitEdge } from "./judge";
import { heldPlaceErrorKm, vacuumKmPerMs, type Observation } from "./model";
import { countryContains, type Country } from "./polygon";

// half the longest part an edge is split into, km: every point of a boundary is this close to a sample
const boundaryCoverKm = 20;
// how far outside a disc a candidate on its circle may compute, km
const circleToleranceKm = 0.001;

interface PhysicalDisc {
  center: Vector3;
  /** the radius as an angle, radians */
  angle: number;
  /** the cosines of the radius widened by boundaryCoverKm and by circleToleranceKm; -2 for a disc that covers the earth */
  coverCos: number;
  toleranceCos: number;
}

const usable = (o: Observation) =>
  -90 <= o.at.lat &&
  o.at.lat <= 90 &&
  -180 <= o.at.lon &&
  o.at.lon <= 180 &&
  0 < o.roundTrip.ms &&
  o.roundTrip.ms !== Infinity &&
  0 <= o.roundTrip.sigmaMs &&
  o.roundTrip.sigmaMs !== Infinity;

/**
 * The IP country is physically possible for the observations, with any one
 * extender's disc left out (V5). An extender's disc is centered at the place
 * of its run with the lowest round trip, `roundTrip.minMs` (else
 * `roundTrip.ms` when it is unknown), and its place error is held as the fit
 * holds it (V3).
 */
export function physicallyPossible(observations: readonly Observation[], country: Country | null): boolean {
  if (country === null || country.polygons.length === 0) {
    return true;
  }
  const discs = physicalDiscs(observations);
  if (discs.length <= 1) {
    return true;
  }

  // (a) the boundary
  for (const polygon of country.polygons) {
    for (const ring of polygon) {
      let possible = false;
      eachBoundarySample(ring, (p) => {
        if (outsideAtMostOne(discs, unitVector(p), true)) {
          possible = true;
          return false;
        }
        return true;
      });
      if (possible) {
        return true;
      }
    }
  }

  // (b) the candidates of every intersection
  const candidates: Vector3[] = [];
  for (const disc of discs) {
    candidates.push(disc.center, onCircle(disc));
  }
  for (let i = 0; i < discs.length; i++) {
    for (let j = i + 1; j < discs.length; j++) {
      candidates.push(...circleCrossings(discs[i], discs[j]));
    }
  }
  for (const candidate of candidates) {
    if (outsideAtMostOne(discs, candidate, false) && countryContains(country, vectorPoint(candidate))) {
      return true;
    }
  }
  return false;
}

/** One disc per extender, in the order extenders first appear, then sorted by radius (stably). */
function physicalDiscs(observations: readonly Observation[]): PhysicalDisc[] {
  interface Extender {
    at: Point;
    yMin: number;
    placeK: number;
  }
  const extenderIds: string[] = [];
  const extenders = new Map<string, Extender>();
  for (const o of observations) {
    if (!usable(o)) {
      continue;
    }
    let y = o.roundTrip.minMs;
    if (!(0 < y) || o.roundTrip.ms < y) {
      y = o.roundTrip.ms;
    }
    const e = extenders.get(o.extenderId);
    if (e === undefined) {
      extenderIds.push(o.extenderId);
      extenders.set(o.extenderId, { at: o.at, yMin: y, placeK: heldPlaceErrorKm(o.placeErrorKm) });
      continue;
    }
    if (y < e.yMin) {
      e.at = o.at;
      e.yMin = y;
      e.placeK = heldPlaceErrorKm(o.placeErrorKm);
    }
  }
  const discs: PhysicalDisc[] = extenderIds.map((id) => {
    const e = extenders.get(id)!;
    const radiusKm = (e.yMin / 2) * vacuumKmPerMs + e.placeK;
    const angle = radiusKm / earthRadiusKm;
    return {
      center: unitVector(e.at),
      angle,
      coverCos: capCos(angle + boundaryCoverKm / earthRadiusKm),
      toleranceCos: capCos(angle + circleToleranceKm / earthRadiusKm),
    };
  });
  // Array.prototype.sort is stable, as Go's SortStableFunc
  discs.sort((a, b) => (a.angle < b.angle ? -1 : b.angle < a.angle ? 1 : 0));
  return discs;
}

/** The cosine a point's dot product with a disc's center must reach to be in it; -2 for a disc that covers the earth. */
function capCos(angle: number): number {
  if (Math.PI <= angle) {
    return -2;
  }
  return Math.cos(angle);
}

/** v is outside at most one disc, widened for a boundary sample or for a candidate. */
function outsideAtMostOne(discs: readonly PhysicalDisc[], v: Vector3, boundary: boolean): boolean {
  let outside = 0;
  for (const disc of discs) {
    const threshold = boundary ? disc.coverCos : disc.toleranceCos;
    if (dot(v, disc.center) < threshold) {
      outside += 1;
      if (1 < outside) {
        return false;
      }
    }
  }
  return true;
}

/** Calls f with each vertex of a closed ring and each point its edges are split at, until f returns false. */
function eachBoundarySample(ring: readonly Point[], f: (p: Point) => boolean): void {
  const n = ring.length;
  if (n === 0) {
    return;
  }
  if (!f(ring[n - 1])) {
    return;
  }
  let prev = ring[n - 1];
  let stop = false;
  for (const cur of ring) {
    splitEdge(prev, cur, (p) => {
      if (!stop && !f(p)) {
        stop = true;
      }
    });
    if (stop) {
      return;
    }
    prev = cur;
  }
}

/** A point on a disc's circle. */
function onCircle(disc: PhysicalDisc): Vector3 {
  // a direction perpendicular to the center: toward the east, or along y at a pole
  let axis = cross([0, 0, 1], disc.center);
  if (norm(axis) < 1e-12) {
    axis = [0, 1, 0];
  } else {
    axis = scale(axis, 1 / norm(axis));
  }
  const angle = Math.min(disc.angle, Math.PI);
  return add(scale(disc.center, Math.cos(angle)), scale(axis, Math.sin(angle)));
}

/** The points where two discs' circles cross: none, or two. */
function circleCrossings(a: PhysicalDisc, b: PhysicalDisc): Vector3[] {
  if (Math.PI <= a.angle || Math.PI <= b.angle) {
    return [];
  }
  const c = dot(a.center, b.center);
  const sin2 = 1 - c * c;
  if (sin2 < 1e-15) {
    return [];
  }
  const cosA = Math.cos(a.angle);
  const cosB = Math.cos(b.angle);
  // x = u a + v b + w (a × b), with x·a = cos α, x·b = cos β, |x| = 1
  const u = (cosA - c * cosB) / sin2;
  const v = (cosB - c * cosA) / sin2;
  const w2 = (1 - (u * u + v * v + 2 * u * v * c)) / sin2;
  if (w2 < 0) {
    return [];
  }
  const w = Math.sqrt(w2);
  const base = add(scale(a.center, u), scale(b.center, v));
  const normal = cross(a.center, b.center);
  return [add(base, scale(normal, w)), add(base, scale(normal, -w))];
}
