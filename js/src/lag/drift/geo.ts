// The earth's geometry (lag.dev/DRIFTTEST.md V2, V6), as connect's lagdrift
// has it (geo.go): great-circle distances and bearings on a sphere, steps in
// local east and north km, and the azimuthal equidistant projection the region
// test works in. Every formula is evaluated in the Go's order, squaring with
// x * x, so the two agree to 10⁻⁶ (V9).

/** A place in degrees: latitude north and longitude east, the longitude in [-180, 180]. */
export interface Point {
  lat: number;
  lon: number;
}

/** A point of an azimuthal equidistant projection: km east and north of its center. */
export interface EastNorth {
  eastKm: number;
  northKm: number;
}

/** The mean earth radius, km. */
export const earthRadiusKm = 6371.0088;

const kmPerDegLat = 110.574;
const kmPerDegLon = 111.32;
export const radPerDeg = Math.PI / 180;
export const degPerRad = 180 / Math.PI;
// a step never moves the latitude past this, so east stays defined
const maxStepLat = 89.9;

// Go's math.Mod: the remainder with the sign of x (JavaScript's % on doubles)
const mod = (x: number, y: number) => x % y;

/**
 * √(p² + q²) as Go's math.Hypot computes it where it has no assembly (arm64):
 * p · √(1 + (q/p)²) with p the larger, so lagdrift's figures and this one's
 * agree to the last bit wherever their inputs do.
 */
export function hypot(p: number, q: number): number {
  p = Math.abs(p);
  q = Math.abs(q);
  if (p === Infinity || q === Infinity) {
    return Infinity;
  }
  if (Number.isNaN(p) || Number.isNaN(q)) {
    return Number.NaN;
  }
  if (p < q) {
    const t = p;
    p = q;
    q = t;
  }
  if (p === 0) {
    return 0;
  }
  q = q / p;
  return p * Math.sqrt(1 + q * q);
}

/** Finite, with the latitude in [-90, 90] and the longitude in [-180, 180]. */
export function pointValid(p: Point): boolean {
  return -90 <= p.lat && p.lat <= 90 && -180 <= p.lon && p.lon <= 180;
}

/** The longitude in [-180, 180). */
export function wrapLon(lon: number): number {
  return mod(mod(lon + 180, 360) + 360, 360) - 180;
}

/** The great-circle distance from a to b, km (haversine). */
export function distanceKm(a: Point, b: Point): number {
  const dLat = (b.lat - a.lat) * radPerDeg;
  const dLon = (b.lon - a.lon) * radPerDeg;
  const sLat = Math.sin(dLat / 2);
  const sLon = Math.sin(dLon / 2);
  const s = sLat * sLat + Math.cos(a.lat * radPerDeg) * Math.cos(b.lat * radPerDeg) * sLon * sLon;
  return 2 * earthRadiusKm * Math.asin(Math.min(1, Math.sqrt(s)));
}

/** The great-circle initial bearing from a to b, degrees clockwise from north in [0, 360). */
export function bearingDeg(a: Point, b: Point): number {
  const p1 = a.lat * radPerDeg;
  const p2 = b.lat * radPerDeg;
  const dl = (b.lon - a.lon) * radPerDeg;
  const y = Math.sin(dl) * Math.cos(p2);
  const x = Math.cos(p1) * Math.sin(p2) - Math.sin(p1) * Math.cos(p2) * Math.cos(dl);
  return mod(Math.atan2(y, x) * degPerRad + 360, 360);
}

/**
 * p moved by east and north km on the local flat earth, as the fit steps
 * (driftFit.ts moveKm). The latitude is held to ±89.9.
 */
export function moveKm(p: Point, eastKm: number, northKm: number): Point {
  const lat = Math.max(-maxStepLat, Math.min(maxStepLat, p.lat + northKm / kmPerDegLat));
  return { lat, lon: wrapLon(p.lon + eastKm / (kmPerDegLon * Math.cos(p.lat * radPerDeg))) };
}

/**
 * p in the azimuthal equidistant projection centered on `origin`: east and
 * north km at p's true distance and initial bearing from it. A fit's offset
 * from the expected place, and the region test (V6), are in it.
 */
export function azimuthalKm(origin: Point, p: Point): EastNorth {
  const d = distanceKm(origin, p);
  const b = bearingDeg(origin, p) * radPerDeg;
  return { eastKm: d * Math.sin(b), northKm: d * Math.cos(b) };
}

/**
 * The inverse of `azimuthalKm`: the place `hypot(eastKm, northKm)` km from
 * `origin` along the bearing `atan2(eastKm, northKm)`. For drawing (V8); the
 * Go model has no need of it.
 */
export function fromAzimuthalKm(origin: Point, eastKm: number, northKm: number): Point {
  const delta = Math.hypot(eastKm, northKm) / earthRadiusKm;
  if (delta === 0) {
    return { lat: origin.lat, lon: origin.lon };
  }
  const theta = Math.atan2(eastKm, northKm);
  const p1 = origin.lat * radPerDeg;
  const sinP1 = Math.sin(p1);
  const cosP1 = Math.cos(p1);
  const sinP2 = Math.max(-1, Math.min(1, sinP1 * Math.cos(delta) + cosP1 * Math.sin(delta) * Math.cos(theta)));
  const lon =
    origin.lon * radPerDeg + Math.atan2(Math.sin(theta) * Math.sin(delta) * cosP1, Math.cos(delta) - sinP1 * sinP2);
  return { lat: Math.asin(sinP2) * degPerRad, lon: wrapLon(lon * degPerRad) };
}

/**
 * Projects many points onto one origin's azimuthal equidistant plane, with
 * the origin's trig computed once: `azimuthalKm` and `distanceKm`'s results.
 */
export class Azimuthal {
  readonly origin: Point;
  private readonly cosLat: number;
  private readonly sinLat: number;

  constructor(origin: Point) {
    this.origin = origin;
    this.cosLat = Math.cos(origin.lat * radPerDeg);
    this.sinLat = Math.sin(origin.lat * radPerDeg);
  }

  /** p's east and north km, and its distance from the origin, km */
  project(p: Point): [number, number, number] {
    const dLat = (p.lat - this.origin.lat) * radPerDeg;
    const dLon = (p.lon - this.origin.lon) * radPerDeg;
    const p2 = p.lat * radPerDeg;
    const cosP2 = Math.cos(p2);
    const sLat = Math.sin(dLat / 2);
    const sLon = Math.sin(dLon / 2);
    const s = sLat * sLat + this.cosLat * cosP2 * sLon * sLon;
    const d = 2 * earthRadiusKm * Math.asin(Math.min(1, Math.sqrt(s)));
    const y = Math.sin(dLon) * cosP2;
    const x = this.cosLat * Math.sin(p2) - this.sinLat * cosP2 * Math.cos(dLon);
    const b = mod(Math.atan2(y, x) * degPerRad + 360, 360) * radPerDeg;
    return [d * Math.sin(b), d * Math.cos(b), d];
  }
}

/** A point on the unit sphere: x toward (0, 0), y toward (0, 90), z toward the north pole. */
export type Vector3 = [number, number, number];

export function unitVector(p: Point): Vector3 {
  const lat = p.lat * radPerDeg;
  const lon = p.lon * radPerDeg;
  const c = Math.cos(lat);
  return [c * Math.cos(lon), c * Math.sin(lon), Math.sin(lat)];
}

export const dot = (a: Vector3, b: Vector3) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
export const cross = (a: Vector3, b: Vector3): Vector3 => [
  a[1] * b[2] - a[2] * b[1],
  a[2] * b[0] - a[0] * b[2],
  a[0] * b[1] - a[1] * b[0],
];
export const scale = (a: Vector3, f: number): Vector3 => [a[0] * f, a[1] * f, a[2] * f];
export const add = (a: Vector3, b: Vector3): Vector3 => [a[0] + b[0], a[1] + b[1], a[2] + b[2]];
export const norm = (a: Vector3) => Math.sqrt(a[0] * a[0] + a[1] * a[1] + a[2] * a[2]);

/** The place a unit vector points at. */
export function vectorPoint(v: Vector3): Point {
  return { lat: Math.atan2(v[2], hypot(v[0], v[1])) * degPerRad, lon: Math.atan2(v[1], v[0]) * degPerRad };
}
