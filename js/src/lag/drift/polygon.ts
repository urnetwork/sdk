// A country's land (lag.dev/DRIFTTEST.md V6, G1, G2), as connect's lagdrift
// tests it (judge.go): inside is in one of its polygons and in none of that
// polygon's holes, tested in degrees by ray casting along the parallel, which
// is how Natural Earth draws its polygons (edges straight in degrees, split at
// the antimeridian, Antarctica closed along the pole). An azimuthal projection
// can't decide it: a country holding the fit's antipode wraps around the
// projection's center.

import { add, degPerRad, distanceKm, earthRadiusKm, hypot, norm, radPerDeg, unitVector, vectorPoint, type Point, type Vector3 } from "./geo";

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

const kmPerDegLat = 110.574;

/** A degree box around a polygon or a country (borders.go). */
export class DegreeBox {
  minLat = Infinity;
  minLon = Infinity;
  maxLat = -Infinity;
  maxLon = -Infinity;

  add(p: Point): void {
    this.minLat = Math.min(this.minLat, p.lat);
    this.minLon = Math.min(this.minLon, p.lon);
    this.maxLat = Math.max(this.maxLat, p.lat);
    this.maxLon = Math.max(this.maxLon, p.lon);
  }

  contains(p: Point): boolean {
    return this.minLat <= p.lat && p.lat <= this.maxLat && this.minLon <= p.lon && p.lon <= this.maxLon;
  }

  /**
   * False only when every point of the box is more than km from p. Two
   * points at latitudes within ±φ and longitudes Δλ apart are at least d
   * apart where sin(d / 2R) = cos φ · sin(Δλ / 2), so the box is widened by
   * km / 110.574 degrees of latitude and by 2 asin(sin(km / 2R) / cos φ) of
   * longitude, φ the widened box's highest latitude, across the antimeridian
   * too.
   */
  near(p: Point, km: number): boolean {
    const latMargin = km / kmPerDegLat;
    if (p.lat < this.minLat - latMargin || this.maxLat + latMargin < p.lat) {
      return false;
    }
    const highLat = Math.max(Math.abs(this.minLat), Math.abs(this.maxLat)) + latMargin;
    if (90 <= highLat || Math.PI * earthRadiusKm <= km) {
      return true;
    }
    const s = Math.sin(km / (2 * earthRadiusKm)) / Math.cos(highLat * radPerDeg);
    if (1 <= s) {
      return true;
    }
    const lonMargin = 2 * Math.asin(s) * degPerRad;
    // the longitude's distance to the box's span, around the earth
    if (this.minLon <= p.lon && p.lon <= this.maxLon) {
      return true;
    }
    const gap = Math.min(lonGap(p.lon, this.minLon), lonGap(p.lon, this.maxLon));
    return gap <= lonMargin;
  }
}

/** The difference between two longitudes the short way around, degrees. */
function lonGap(a: number, b: number): number {
  const d = Math.abs(a - b) % 360;
  return Math.min(d, 360 - d);
}

/** Bounds on one of a country's polygons, for skipping the far ones (judge.go). */
export interface PolygonBound {
  /** the outline's degree box */
  box: DegreeBox;
  /** every boundary point, and every chord the region test draws between them, is within radiusKm of center */
  center: Point;
  radiusKm: number;
}

// The bounds of the countries Borders made, as lagdrift's Country.index: a
// country without them is searched whole, to the same result.
const countryIndexes = new WeakMap<Country, PolygonBound[]>();

/**
 * Bounds on a country's polygons. A lon/lat-straight edge stays within half
 * its length, at most √(Δlat² + Δlon²) degrees of 111.2 km, of its nearer
 * end, and a chord between split points in the azimuthal plane, within
 * 10,000 km, strays from the edge by less than 32 km more.
 */
function newCountryIndex(polygons: readonly (readonly (readonly Point[])[])[]): PolygonBound[] {
  return polygons.map((polygon) => {
    const bound: PolygonBound = { box: new DegreeBox(), center: { lat: 0, lon: 0 }, radiusKm: 0 };
    if (polygon.length === 0 || polygon[0].length === 0) {
      bound.radiusKm = Infinity;
      return bound;
    }
    let sum: Vector3 = [0, 0, 0];
    for (const ring of polygon) {
      for (const p of ring) {
        sum = add(sum, unitVector(p));
      }
    }
    for (const p of polygon[0]) {
      bound.box.add(p);
    }
    if (norm(sum) < 1e-9) {
      bound.radiusKm = Infinity;
      return bound;
    }
    bound.center = vectorPoint(sum);
    let vertexKm = 0.0;
    let halfEdgeKm = 0.0;
    for (const ring of polygon) {
      ring.forEach((p, j) => {
        vertexKm = Math.max(vertexKm, distanceKm(bound.center, p));
        const q = ring[(j + 1) % ring.length];
        halfEdgeKm = Math.max(halfEdgeKm, (hypot(q.lat - p.lat, q.lon - p.lon) * 111.2) / 2);
      });
    }
    bound.radiusKm = vertexKm + halfEdgeKm + 32;
    return bound;
  });
}

/** Indexes a country Borders made, so the tests below skip its far polygons. */
export function indexCountry(country: Country): void {
  countryIndexes.set(country, newCountryIndex(country.polygons));
}

/** A country's polygon bounds, when Borders made it. */
export function countryIndex(country: Country): readonly PolygonBound[] | undefined {
  return countryIndexes.get(country);
}

/** p is inside one of the country's polygons and in none of that polygon's holes. */
export function countryContains(country: Country, p: Point): boolean {
  const index = countryIndexes.get(country);
  for (let i = 0; i < country.polygons.length; i++) {
    if (index !== undefined && !index[i].box.contains(p)) {
      continue;
    }
    if (polygonContains(country.polygons[i], p)) {
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
