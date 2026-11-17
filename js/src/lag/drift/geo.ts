// Geodesy for the drift model (lag.dev/DRIFTTEST.md V2, V6): great-circle
// distance and bearing on a spherical earth, and the azimuthal equidistant
// projection the fit steps in and the verdict judges in.

/** A place, in degrees. */
export interface Point {
  lat: number;
  lon: number;
}

/** A point of the azimuthal equidistant projection: km east and north of its center. */
export interface EastNorth {
  eastKm: number;
  northKm: number;
}

/** The earth's mean radius (IUGG), km. */
export const earthRadiusKm = 6371.0088;

export const RAD = Math.PI / 180;
export const DEG = 180 / Math.PI;

/** A longitude in [-180, 180). */
export function wrapLon(lon: number): number {
  return ((((lon + 180) % 360) + 360) % 360) - 180;
}

/** The great-circle distance from a to b, km (haversine). */
export function distanceKm(a: Point, b: Point): number {
  const dLat = (b.lat - a.lat) * RAD;
  const dLon = (b.lon - a.lon) * RAD;
  const sLat = Math.sin(dLat / 2);
  const sLon = Math.sin(dLon / 2);
  const s = sLat * sLat + Math.cos(a.lat * RAD) * Math.cos(b.lat * RAD) * sLon * sLon;
  return 2 * earthRadiusKm * Math.asin(Math.min(1, Math.sqrt(s)));
}

/** The great-circle initial bearing from a to b, radians clockwise from north, in (-π, π]. */
export function bearingRad(a: Point, b: Point): number {
  const p1 = a.lat * RAD;
  const p2 = b.lat * RAD;
  const dl = (b.lon - a.lon) * RAD;
  const y = Math.sin(dl) * Math.cos(p2);
  const x = Math.cos(p1) * Math.sin(p2) - Math.sin(p1) * Math.cos(p2) * Math.cos(dl);
  return Math.atan2(y, x);
}

/** The great-circle initial bearing from a to b, degrees clockwise from north, in [0, 360). */
export function bearingDeg(a: Point, b: Point): number {
  const deg = bearingRad(a, b) * DEG;
  return deg < 0 ? deg + 360 : deg;
}

/**
 * p in the azimuthal equidistant projection centered on `origin`: its true
 * distance from the origin along its initial bearing.
 */
export function toEastNorth(origin: Point, p: Point): EastNorth {
  const d = distanceKm(origin, p);
  if (d === 0) {
    return { eastKm: 0, northKm: 0 };
  }
  const beta = bearingRad(origin, p);
  return { eastKm: d * Math.sin(beta), northKm: d * Math.cos(beta) };
}

/**
 * The inverse of `toEastNorth`: the place `hypot(eastKm, northKm)` km from
 * `origin` along the bearing `atan2(eastKm, northKm)`.
 */
export function fromEastNorth(origin: Point, eastKm: number, northKm: number): Point {
  const delta = Math.hypot(eastKm, northKm) / earthRadiusKm;
  if (delta === 0) {
    return { lat: origin.lat, lon: origin.lon };
  }
  const theta = Math.atan2(eastKm, northKm);
  const p1 = origin.lat * RAD;
  const sinP1 = Math.sin(p1);
  const cosP1 = Math.cos(p1);
  const sinP2 = Math.max(-1, Math.min(1, sinP1 * Math.cos(delta) + cosP1 * Math.sin(delta) * Math.cos(theta)));
  const lon =
    origin.lon * RAD + Math.atan2(Math.sin(theta) * Math.sin(delta) * cosP1, Math.cos(delta) - sinP1 * sinP2);
  return { lat: Math.asin(sinP2) * DEG, lon: wrapLon(lon * DEG) };
}

/** A unit vector from the earth's center through p. */
export function toUnitVector(p: Point): [number, number, number] {
  const lat = p.lat * RAD;
  const lon = p.lon * RAD;
  const c = Math.cos(lat);
  return [c * Math.cos(lon), c * Math.sin(lon), Math.sin(lat)];
}

/** The place a (not necessarily unit) vector from the earth's center points at. */
export function fromVector(v: readonly [number, number, number]): Point {
  return { lat: Math.atan2(v[2], Math.hypot(v[0], v[1])) * DEG, lon: wrapLon(Math.atan2(v[1], v[0]) * DEG) };
}
