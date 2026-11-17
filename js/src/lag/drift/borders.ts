// Country borders by GeoLite2's country code (lag.dev/DRIFTTEST.md G1, G2), and
// the drift country (V6): the country containing a fit, else the nearest
// within a distance, else none. As connect's lagdrift has them (borders.go).

import { pointValid, type Point } from "./geo";
import type { Cov2 } from "./fit";
import { BoundarySearch } from "./judge";
import { DegreeBox, indexCountry, polygonContains, type Country } from "./polygon";

/** The drift country is the nearest within this far when none contains the fit, km (V6). */
export const driftCountryWithinKm = 300;

const identity: Cov2 = [
  [1, 0],
  [0, 1],
];

function validCode(code: string): boolean {
  return /^[a-z]{2}$/.test(code);
}

/** Countries by code (G1, G2). Read-only once made. */
export class Borders {
  private readonly geometryVersion: string;
  // sorted by code
  private readonly countries: Country[] = [];
  // per country, a box around each polygon's outline, and around all of them
  private readonly polygonBoxes: DegreeBox[][] = [];
  private readonly countryBoxes: DegreeBox[] = [];
  private readonly byCode = new Map<string, Country>();

  /**
   * Borders of the given countries, merging the polygons of a code given
   * more than once. Codes are GeoLite2's: two lower-case letters; any other
   * throws a TypeError.
   */
  constructor(version: string, countries: Iterable<Country>) {
    this.geometryVersion = version;
    const codeCountries = new Map<string, Country>();
    const codes: string[] = [];
    for (const country of countries) {
      if (!validCode(country.code)) {
        throw new TypeError(`drift borders: a country code is two lower-case letters, not ${JSON.stringify(country.code)}`);
      }
      let merged = codeCountries.get(country.code);
      if (merged === undefined) {
        merged = { code: country.code, polygons: [] };
        codeCountries.set(country.code, merged);
        codes.push(country.code);
      }
      merged.polygons.push(...country.polygons);
    }
    codes.sort();
    for (const code of codes) {
      const country = codeCountries.get(code)!;
      const boxes = country.polygons.map(() => new DegreeBox());
      const countryBox = new DegreeBox();
      country.polygons.forEach((polygon, i) => {
        if (0 < polygon.length) {
          for (const p of polygon[0]) {
            boxes[i].add(p);
            countryBox.add(p);
          }
        }
      });
      indexCountry(country);
      this.countries.push(country);
      this.polygonBoxes.push(boxes);
      this.countryBoxes.push(countryBox);
      this.byCode.set(code, country);
    }
  }

  /**
   * The geometry's version, e.g. ne-admin0-50m-v5.1.2, from the file's header:
   * what a verdict records as geometry_version (G1, P4).
   */
  version(): string {
    return this.geometryVersion;
  }

  /** Every country's code, sorted. */
  codes(): string[] {
    return this.countries.map((country) => country.code);
  }

  /** The country of a code, in either case, or null when there is none. It is the borders' own: don't change it. */
  country(code: string): Country | null {
    return this.byCode.get(code.toLowerCase()) ?? null;
  }

  /**
   * The drift country (V6): the code of the country containing p, else of
   * the nearest country whose boundary is within `withinKm`, else "" (at
   * sea). Where countries overlap, as disputed areas given to each claimant
   * do, the first code in order wins. The distance is measured to the
   * boundary as the region test measures it, in the azimuthal plane centered
   * on p.
   */
  countryAt(p: Point, withinKm: number = driftCountryWithinKm): string {
    if (!pointValid(p)) {
      return "";
    }
    for (let i = 0; i < this.countries.length; i++) {
      if (!this.countryBoxes[i].contains(p)) {
        continue;
      }
      const country = this.countries[i];
      for (let j = 0; j < country.polygons.length; j++) {
        if (this.polygonBoxes[i][j].contains(p) && polygonContains(country.polygons[j], p)) {
          return country.code;
        }
      }
    }
    if (!(0 < withinKm)) {
      return "";
    }
    let bestCode = "";
    let bestKm = Infinity;
    for (let i = 0; i < this.countries.length; i++) {
      if (!this.countryBoxes[i].near(p, withinKm)) {
        continue;
      }
      const country = this.countries[i];
      const search = new BoundarySearch(p, identity);
      for (let j = 0; j < country.polygons.length; j++) {
        if (!this.polygonBoxes[i][j].near(p, withinKm)) {
          continue;
        }
        for (const ring of country.polygons[j]) {
          search.ring(ring);
        }
      }
      if (search.found && search.best < bestKm) {
        bestKm = search.best;
        bestCode = country.code;
      }
    }
    return bestKm <= withinKm ? bestCode : "";
  }
}
