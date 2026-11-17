// Country polygons (lag.dev/DRIFTTEST.md G1, G2): containment in longitude
// and latitude, as Natural Earth draws them. Its rings never cross the
// antimeridian, and a ring around a pole closes along it, so a planar test
// in degrees is exact for them.

import type { Point } from "./geo";

/** A country's borders: each polygon's first ring is its outline, the rest are holes. */
export interface Country {
  /** ISO 3166-1 alpha-2 as GeoLite2 gives it, lower case */
  code: string;
  polygons: Point[][][];
}

/** Whether p is inside a ring, by the crossings of a ray east of it. */
export function ringContains(ring: readonly Point[], p: Point): boolean {
  let inside = false;
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const a = ring[i];
    const b = ring[j];
    if (a.lat > p.lat !== b.lat > p.lat) {
      const lon = a.lon + ((p.lat - a.lat) * (b.lon - a.lon)) / (b.lat - a.lat);
      if (p.lon < lon) {
        inside = !inside;
      }
    }
  }
  return inside;
}

/** Whether p is inside a polygon's outline and outside its holes. */
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

/** Whether p is inside any of a country's polygons. */
export function countryContains(country: Country, p: Point): boolean {
  return country.polygons.some((polygon) => polygonContains(polygon, p));
}

/** The point of the segment from a to b nearest the origin. */
export function nearestOnSegment(ax: number, ay: number, bx: number, by: number): { x: number; y: number } {
  const dx = bx - ax;
  const dy = by - ay;
  const length2 = dx * dx + dy * dy;
  const t = length2 > 0 ? Math.max(0, Math.min(1, -(ax * dx + ay * dy) / length2)) : 0;
  return { x: ax + t * dx, y: ay + t * dy };
}
