// Country borders by code (lag.dev/DRIFTTEST.md G1, G2), and the drift
// country (V6): the country containing a place, else the nearest within a
// distance, else none.

import { earthRadiusKm, distanceKm, fromVector, toEastNorth, type Point } from "./geo";
import { nearestOnSegment, polygonContains, type Country } from "./polygon";

/** V6: the drift country is the nearest one within this far when none contains the fit, km. */
export const driftCountryWithinKm = 300;

interface Entry {
  code: string;
  polygon: Point[][];
  minLat: number;
  maxLat: number;
  minLon: number;
  maxLon: number;
  /** a cap around the polygon's vertices: its center, and its radius in km (Infinity for none) */
  center: Point;
  radiusKm: number;
}

function entry(code: string, polygon: Point[][]): Entry {
  let minLat = Infinity;
  let maxLat = -Infinity;
  let minLon = Infinity;
  let maxLon = -Infinity;
  let x = 0;
  let y = 0;
  let z = 0;
  for (const ring of polygon) {
    for (const p of ring) {
      minLat = Math.min(minLat, p.lat);
      maxLat = Math.max(maxLat, p.lat);
      minLon = Math.min(minLon, p.lon);
      maxLon = Math.max(maxLon, p.lon);
      const c = Math.cos((p.lat * Math.PI) / 180);
      x += c * Math.cos((p.lon * Math.PI) / 180);
      y += c * Math.sin((p.lon * Math.PI) / 180);
      z += Math.sin((p.lat * Math.PI) / 180);
    }
  }
  const center = fromVector([x, y, z]);
  let radiusKm = 0;
  for (const ring of polygon) {
    for (const p of ring) {
      radiusKm = Math.max(radiusKm, distanceKm(center, p));
    }
  }
  // a cap past a quarter of the earth, or about a near-even spread, bounds nothing
  if (!(Math.hypot(x, y, z) > 1e-9) || radiusKm > (earthRadiusKm * Math.PI) / 2) {
    radiusKm = Infinity;
  } else {
    // edges straight in degrees can bow a little past their ends
    radiusKm = radiusKm * 1.01 + 1;
  }
  return { code, polygon, minLat, maxLat, minLon, maxLon, center, radiusKm };
}

/** The distance from p to the nearest point of a polygon's rings, km. */
function distanceToBoundaryKm(polygon: readonly (readonly Point[])[], p: Point): number {
  let best = Infinity;
  for (const ring of polygon) {
    const n = ring.length;
    if (n === 0) {
      continue;
    }
    const first = toEastNorth(p, ring[0]);
    let a = first;
    for (let i = 1; i <= n; i++) {
      const b = i < n ? toEastNorth(p, ring[i]) : first;
      const q = nearestOnSegment(a.eastKm, a.northKm, b.eastKm, b.northKm);
      best = Math.min(best, Math.hypot(q.x, q.y));
      a = b;
    }
  }
  return best;
}

/** Countries by code (G1, G2). */
export class Borders {
  /** the map's version, e.g. its Natural Earth release */
  readonly version: string;
  private readonly byCode = new Map<string, Country>();
  private readonly entries: Entry[] = [];

  constructor(countries: Iterable<Country>, version = "") {
    this.version = version;
    for (const country of countries) {
      const code = country.code.toLowerCase();
      const existing = this.byCode.get(code);
      if (existing === undefined) {
        this.byCode.set(code, { code, polygons: country.polygons.slice() });
      } else {
        existing.polygons.push(...country.polygons);
      }
      for (const polygon of country.polygons) {
        if (polygon.length > 0 && polygon[0].length > 0) {
          this.entries.push(entry(code, polygon));
        }
      }
    }
  }

  /** Every country's code, lower case. */
  codes(): string[] {
    return [...this.byCode.keys()];
  }

  /** A country by its code, in either case; `null` when the map has none. */
  country(code: string): Country | null {
    return this.byCode.get(code.toLowerCase()) ?? null;
  }

  /**
   * V6: the code of the country containing p, else of the nearest one within
   * `withinKm`, else "" (at sea).
   */
  countryAt(p: Point, withinKm: number = driftCountryWithinKm): string {
    for (const e of this.entries) {
      if (
        e.minLat <= p.lat &&
        p.lat <= e.maxLat &&
        e.minLon <= p.lon &&
        p.lon <= e.maxLon &&
        polygonContains(e.polygon, p)
      ) {
        return e.code;
      }
    }
    let best = "";
    let bestKm = Infinity;
    for (const e of this.entries) {
      const lowerBoundKm = e.radiusKm === Infinity ? 0 : distanceKm(p, e.center) - e.radiusKm;
      if (lowerBoundKm > Math.min(withinKm, bestKm)) {
        continue;
      }
      const km = distanceToBoundaryKm(e.polygon, p);
      if (km <= withinKm && km < bestKm) {
        best = e.code;
        bestKm = km;
      }
    }
    return best;
  }
}
