// A country's land (lag.dev/DRIFTTEST.md V6, G1, G2), as connect's lagdrift
// tests it (judge.go): inside is in one of its polygons and in none of that
// polygon's holes, tested in degrees by ray casting along the parallel, which
// is how Natural Earth draws its polygons (edges straight in degrees, split at
// the antimeridian, Antarctica closed along the pole). An azimuthal projection
// can't decide it: a country holding the fit's antipode wraps around the
// projection's center.

import type { Point } from "./geo";

/**
 * A country's land: polygons whose first ring is the outline and whose other
 * rings are holes. A ring's last point may repeat its first or not. Its
 * edges are straight in degrees and never cross the antimeridian.
 */
export interface Country {
  /** GeoLite2's code, lower case */
  code: string;
  polygons: Point[][][];
}

/** p is inside one of the country's polygons and in none of that polygon's holes. */
export function countryContains(country: Country, p: Point): boolean {
  for (const polygon of country.polygons) {
    if (polygonContains(polygon, p)) {
      return true;
    }
  }
  return false;
}

export function polygonContains(polygon: readonly (readonly Point[])[], p: Point): boolean {
  if (polygon.length === 0 || !ringContains(polygon[0], p)) {
    return false;
  }
  for (let h = 1; h < polygon.length; h++) {
    if (ringContains(polygon[h], p)) {
      return false;
    }
  }
  return true;
}

/** Ray casting along the parallel toward the east, with edges straight in degrees. */
export function ringContains(ring: readonly Point[], p: Point): boolean {
  let inside = false;
  const n = ring.length;
  for (let i = 0, j = n - 1; i < n; j = i, i++) {
    const a = ring[i];
    const b = ring[j];
    if (p.lat < a.lat !== p.lat < b.lat) {
      const x = ((b.lon - a.lon) * (p.lat - a.lat)) / (b.lat - a.lat) + a.lon;
      if (p.lon < x) {
        inside = !inside;
      }
    }
  }
  return inside;
}
