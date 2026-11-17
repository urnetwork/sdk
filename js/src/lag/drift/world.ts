// G2: the simplified world map, for display and the demo's local verdict
// (lag.dev/DRIFTTEST.md G2, I5, W8). It is world.generated.json, written by
// connect's `go run ./lagdrift/cmd/drift-borders`, and it is imported
// dynamically, so a page that never draws a border, such as the speed test,
// never loads it. It never decides a real token's verdict: the api's 1:10m
// borders do (G1).

import { Borders } from "./borders";
import type { Point } from "./geo";
import type { Country } from "./polygon";

/** world.generated.json: each ring a flat list of longitude and latitude pairs, as integers in units of 1 / `scale` degree. */
export interface DriftWorldData {
  version?: string;
  /** units per degree; 1 when the coordinates are degrees */
  scale?: number;
  countries: { code: string; polygons: number[][][] }[];
}

/** Reads the world map's data into borders. */
export function readBorders(data: DriftWorldData): Borders {
  if (typeof data !== "object" || data === null || !Array.isArray(data.countries)) {
    throw new TypeError("drift world: no countries");
  }
  const scale = data.scale ?? 1;
  if (!(Number.isFinite(scale) && scale > 0)) {
    throw new TypeError(`drift world: bad scale ${scale}`);
  }
  const countries: Country[] = data.countries.map(({ code, polygons }) => ({
    code,
    polygons: polygons.map((rings) =>
      rings.map((flat) => {
        const ring: Point[] = [];
        for (let i = 0; i + 1 < flat.length; i += 2) {
          ring.push({ lon: flat[i] / scale, lat: flat[i + 1] / scale });
        }
        return ring;
      }),
    ),
  }));
  return new Borders(countries, data.version ?? "");
}

let world: Promise<Borders> | undefined;

/**
 * G2: the simplified world map, loaded once, on first use. A failed load is
 * forgotten, so the next call tries again.
 */
export function loadDriftWorld(): Promise<Borders> {
  if (world === undefined) {
    const loading = import("./world.generated.json", { with: { type: "json" } }).then((module) =>
      readBorders((module as { default: DriftWorldData }).default),
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
