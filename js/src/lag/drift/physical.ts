// V5's physical check (lag.dev/DRIFTTEST.md V5). Nothing travels faster than
// light in a vacuum, so the visitor is within
//
//   Rᵢ = (yᵢ,min / 2) × 299.79 km + s_ext,i
//
// of each extender, yᵢ,min being its lowest round trip in any run. The IP
// country is physically possible when some point of it lies in every disc,
// with any one disc dropped: one misplaced extender or broken path can't
// make it impossible on its own.
//
// The test is exact on the sphere. A disc is a spherical cap, and the
// intersection S of caps meets the country exactly when (a) S meets one of
// its borders, each edge taken as a great-circle arc, or (b) a component of S
// lies inside the country, in which case a corner of that component (where
// two cap circles cross) or, for a component with no corners, a point of a
// whole cap circle lies inside it.

import { earthRadiusKm, fromVector, toUnitVector, type Point } from "./geo";
import { placeErrorKm, vacuumKmPerMs, type Observation } from "./model";
import { countryContains, type Country } from "./polygon";

type Vec = [number, number, number];

const EPS = 1e-12;

interface Cap {
  center: Vec;
  /** the cosine of its angular radius */
  cosRadius: number;
}

const dot = (a: Vec, b: Vec) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
const cross = (a: Vec, b: Vec): Vec => [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
const norm = (a: Vec) => Math.hypot(a[0], a[1], a[2]);
const scale = (a: Vec, s: number): Vec => [a[0] * s, a[1] * s, a[2] * s];

/** V5: each extender's disc, km: its lowest round trip at the speed of light, plus its place error. */
export function reachKm(observations: readonly Observation[]): Map<string, { at: Point; radiusKm: number }> {
  const discs = new Map<string, { at: Point; radiusKm: number }>();
  for (const o of observations) {
    if (!Number.isFinite(o.roundTrip.ms)) {
      continue;
    }
    const radiusKm = (o.roundTrip.ms / 2) * vacuumKmPerMs + placeErrorKm(o.placeErrorKm);
    const disc = discs.get(o.extenderId);
    if (disc === undefined || radiusKm < disc.radiusKm) {
      discs.set(o.extenderId, { at: o.at, radiusKm });
    }
  }
  return discs;
}

/** the parameter intervals of the arc a → b (angle `length`) inside a cap */
function arcInCap(a: Vec, w: Vec, length: number, cap: Cap): [number, number][] {
  // a point of the arc is a cos t + w sin t, and its dot with the center is M cos(t − φ)
  const A = dot(a, cap.center);
  const B = dot(w, cap.center);
  const M = Math.hypot(A, B);
  const c = cap.cosRadius;
  if (M < EPS) {
    return c <= EPS ? [[0, length]] : [];
  }
  if (c / M > 1 + EPS) {
    return [];
  }
  if (c / M <= -1) {
    return [[0, length]];
  }
  const alpha = Math.acos(Math.max(-1, Math.min(1, c / M)));
  const phi = Math.atan2(B, A);
  const out: [number, number][] = [];
  for (const shift of [-2 * Math.PI, 0, 2 * Math.PI]) {
    const lo = Math.max(0, phi - alpha + shift);
    const hi = Math.min(length, phi + alpha + shift);
    if (lo <= hi) {
      out.push([lo, hi]);
    }
  }
  return out;
}

function intersectIntervals(x: readonly [number, number][], y: readonly [number, number][]): [number, number][] {
  const out: [number, number][] = [];
  for (const [a0, a1] of x) {
    for (const [b0, b1] of y) {
      const lo = Math.max(a0, b0);
      const hi = Math.min(a1, b1);
      if (lo <= hi + EPS) {
        out.push([lo, Math.max(lo, hi)]);
      }
    }
  }
  return out;
}

const inCap = (x: Vec, cap: Cap) => dot(x, cap.center) >= cap.cosRadius - 1e-9;

/** the points where two cap circles cross */
function circleCrossings(p: Cap, q: Cap): Vec[] {
  const s = dot(p.center, q.center);
  const d = 1 - s * s;
  if (d < EPS) {
    return [];
  }
  const alpha = (p.cosRadius - q.cosRadius * s) / d;
  const beta = (q.cosRadius - p.cosRadius * s) / d;
  const base: Vec = [
    alpha * p.center[0] + beta * q.center[0],
    alpha * p.center[1] + beta * q.center[1],
    alpha * p.center[2] + beta * q.center[2],
  ];
  const rest = 1 - dot(base, base);
  if (rest < -1e-9) {
    return [];
  }
  const axis = cross(p.center, q.center);
  const gamma = Math.sqrt(Math.max(0, rest) / dot(axis, axis));
  return [
    [base[0] + gamma * axis[0], base[1] + gamma * axis[1], base[2] + gamma * axis[2]],
    [base[0] - gamma * axis[0], base[1] - gamma * axis[1], base[2] - gamma * axis[2]],
  ];
}

/** a point of a cap's circle */
function pointOnCircle(cap: Cap): Vec {
  const c = cap.center;
  const helper: Vec = Math.abs(c[2]) < 0.9 ? [0, 0, 1] : [1, 0, 0];
  const u = cross(c, helper);
  const unit = scale(u, 1 / norm(u));
  const sin = Math.sqrt(Math.max(0, 1 - cap.cosRadius * cap.cosRadius));
  return [
    cap.cosRadius * c[0] + sin * unit[0],
    cap.cosRadius * c[1] + sin * unit[1],
    cap.cosRadius * c[2] + sin * unit[2],
  ];
}

/**
 * V5: whether the IP country can hold the visitor at all: some point of it
 * within every extender's reach, with any single extender's dropped.
 */
export function physicallyPossible(observations: readonly Observation[], country: Country): boolean {
  const caps: Cap[] = [];
  for (const { at, radiusKm } of reachKm(observations).values()) {
    const angle = radiusKm / earthRadiusKm;
    // a disc that covers the earth rules nothing out
    if (angle < Math.PI) {
      caps.push({ center: toUnitVector(at), cosRadius: Math.cos(angle) });
    }
  }
  // with at most one disc, dropping it leaves the whole earth
  if (caps.length <= 1 || country.polygons.length === 0) {
    return country.polygons.length > 0;
  }

  // (a) a border edge in every cap but at most one
  for (const polygon of country.polygons) {
    for (const ring of polygon) {
      const points = ring.map((p) => toUnitVector(p));
      for (let i = 0; i < points.length; i++) {
        const a = points[i];
        const b = points[i + 1 < points.length ? i + 1 : 0];
        const axis = cross(a, b);
        const sin = norm(axis);
        const length = Math.atan2(sin, dot(a, b));
        let intervals: [number, number][][];
        if (sin < EPS) {
          // a degenerate edge is its first point
          intervals = caps.map((cap) => (inCap(a, cap) ? [[0, 0] as [number, number]] : []));
        } else {
          const ab = dot(a, b);
          const along = [b[0] - ab * a[0], b[1] - ab * a[1], b[2] - ab * a[2]] as Vec;
          const w = scale(along, 1 / norm(along));
          intervals = caps.map((cap) => arcInCap(a, w, length, cap));
        }
        const missed = intervals.flatMap((x, c) => (x.length === 0 ? [c] : []));
        if (missed.length > 1) {
          continue;
        }
        // the edge's points in every cap but `drop`
        const meets = (drop: number) => {
          let common: [number, number][] = [[0, sin < EPS ? 0 : length]];
          for (let c = 0; c < caps.length && common.length > 0; c++) {
            if (c !== drop) {
              common = intersectIntervals(common, intervals[c]);
            }
          }
          return common.length > 0;
        };
        if (missed.length === 1) {
          if (meets(missed[0])) {
            return true;
          }
          continue;
        }
        if (meets(-1)) {
          return true;
        }
        for (let drop = 0; drop < caps.length; drop++) {
          if (meets(drop)) {
            return true;
          }
        }
      }
    }
  }

  // (b) a corner of the caps' intersection, or a whole circle of it, inside the country
  const candidates: Vec[] = [];
  for (let i = 0; i < caps.length; i++) {
    candidates.push(pointOnCircle(caps[i]));
    for (let j = i + 1; j < caps.length; j++) {
      candidates.push(...circleCrossings(caps[i], caps[j]));
    }
  }
  for (const x of candidates) {
    let outside = 0;
    for (const cap of caps) {
      if (!inCap(x, cap) && ++outside > 1) {
        break;
      }
    }
    if (outside <= 1 && countryContains(country, fromVector(x))) {
      return true;
    }
  }
  return false;
}
