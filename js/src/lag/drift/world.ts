// G2: the simplified world map, for display and the demo's local verdict
// (lag.dev/DRIFTTEST.md G2, I5, W8): Natural Earth's Admin 0 at 1:50m,
// simplified at about 5 km and quantized, written by connect's
// `go run ./lagdrift/cmd/drift-borders` as world.generated.json and read as
// lagdrift's ReadWorld reads it (world.go). It is imported dynamically, so a
// page that never draws a border, such as the speed test, never loads it. It
// never decides a real token's verdict: the api's 1:10m borders do (G1).
//
//   {"format": "lagdrift-world/1",
//    "version": "ne-admin0-50m-v5.1.2",   the geometry's version
//    "simplifyKm": 5,                      the simplification's tolerance, km
//    "scale": 1000,                        units per degree
//    "countries": [                        sorted by code
//      {"code": "ad",
//       "polygons": [                      each polygon a list of rings, the
//         [[lon, lat, lon, lat, …], …]     first the outline and the rest
//       ]}, …]}                            holes
//
// Each ring is a flat list of integers, longitude then latitude for each
// point, in 1 / scale degree, at least 3 points, closed implicitly.

import { Borders } from "./borders";
import { pointValid, type Point } from "./geo";
import type { Country } from "./polygon";

/** The world map's format field. */
export const worldFormat = "lagdrift-world/1";

/** world.generated.json. */
export interface DriftWorldData {
  format: string;
  version: string;
  simplifyKm?: number;
  /** units per degree */
  scale: number;
  countries: { code: string; polygons: number[][][] }[];
}

// a ring has at least this many points
const minRingPoints = 3;

const invalid = (why: string) => new TypeError(`drift world: ${why}`);

/** Reads the world map into borders. Its points are the integers divided by the scale. */
export function readWorld(data: DriftWorldData | string): Borders {
  const file = (typeof data === "string" ? JSON.parse(data) : data) as Partial<DriftWorldData> | null;
  if (typeof file !== "object" || file === null) {
    throw invalid("not a world map");
  }
  if (file.format !== worldFormat) {
    throw invalid(`format ${JSON.stringify(file.format)}, not ${JSON.stringify(worldFormat)}`);
  }
  const scale = file.scale as number;
  if (!(0 < scale) || scale === Infinity) {
    throw invalid(`scale ${scale}`);
  }
  const countries: Country[] = (file.countries ?? []).map((wc) => {
    const polygons = (wc.polygons ?? []).map((wp) => {
      if (wp.length === 0) {
        throw invalid(`a polygon of ${wc.code} has no rings`);
      }
      return wp.map((flat) => {
        if (flat.length % 2 !== 0 || flat.length < 2 * minRingPoints) {
          throw invalid(`a ring of ${wc.code} has ${flat.length} numbers`);
        }
        const ring: Point[] = [];
        for (let i = 0; i < flat.length; i += 2) {
          const p = { lat: flat[i + 1] / scale, lon: flat[i] / scale };
          if (!pointValid(p)) {
            throw invalid(`a point of ${wc.code} is out of range`);
          }
          ring.push(p);
        }
        return ring;
      });
    });
    return { code: wc.code, polygons };
  });
  return new Borders(file.version ?? "", countries);
}

let world: Promise<Borders> | undefined;

/**
 * G2: the simplified world map, loaded once, on first use. A failed load is
 * forgotten, so the next call tries again.
 */
export function loadDriftWorld(): Promise<Borders> {
  if (world === undefined) {
    const loading = import("./world.generated.json", { with: { type: "json" } }).then((module) =>
      readWorld((module as { default: DriftWorldData }).default),
    );
    world = loading;
    loading.catch(() => {
      if (world === loading) {
        world = undefined;
      }
    });
  }
  return world;
}
