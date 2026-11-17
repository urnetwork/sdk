import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

// Borders and the drift country (lag.dev/DRIFTTEST.md V6, G2), the simplified
// world map (G2, I5) and the demo token's local verdict (W8, V7).

registerTsResolve();
const drift = await import("../../src/lag/drift/index.ts");
const fx = await import("./drift_fixtures.ts");

type Point = import("../../src/lag/drift/index.ts").Point;

const close = (actual: number, expected: number, tolerance: number, message?: string) =>
  assert.ok(Math.abs(actual - expected) <= tolerance, `${message ?? ""} ${actual} is not within ${tolerance} of ${expected}`);

const atlantic: Point = { lat: 40, lon: -40 };

test("borders: a country by code in either case, and every code", () => {
  const borders = new drift.Borders([fx.squareCountry("AA", fx.CHICAGO, 100), fx.squareCountry("bb", fx.MADRID, 100)], "test-1");
  assert.equal(borders.version, "test-1");
  assert.deepEqual(borders.codes().sort(), ["aa", "bb"]);
  assert.equal(borders.country("aa")?.code, "aa");
  assert.equal(borders.country("AA")?.code, "aa");
  assert.equal(borders.country("cc"), null);
  // a code listed twice is one country with both polygons
  const twice = new drift.Borders([fx.squareCountry("aa", fx.CHICAGO, 100), fx.squareCountry("aa", fx.MADRID, 100)]);
  assert.equal(twice.country("aa")!.polygons.length, 2);
  assert.equal(twice.countryAt(fx.MADRID, 0), "aa");
});

test("V6: the drift country is the one containing the fit, else the nearest within 300 km, else none", () => {
  const borders = new drift.Borders([
    fx.squareCountry("aa", fx.CHICAGO, 100),
    fx.squareCountry("bb", drift.fromEastNorth(fx.CHICAGO, 500, 0), 100),
  ]);
  assert.equal(drift.driftCountryWithinKm, 300);
  assert.equal(borders.countryAt(fx.CHICAGO, 300), "aa");
  assert.equal(borders.countryAt(drift.fromEastNorth(fx.CHICAGO, 500, 50), 300), "bb");
  // 150 km east of aa's border and 150 km west of bb's: the nearer by a little
  assert.equal(borders.countryAt(drift.fromEastNorth(fx.CHICAGO, 240, 0), 300), "aa");
  assert.equal(borders.countryAt(drift.fromEastNorth(fx.CHICAGO, 260, 0), 300), "bb");
  // 250 km north of aa: within 300 km, not within 200
  assert.equal(borders.countryAt(drift.fromEastNorth(fx.CHICAGO, 0, 350), 300), "aa");
  assert.equal(borders.countryAt(drift.fromEastNorth(fx.CHICAGO, 0, 350), 200), "");
  assert.equal(borders.countryAt(drift.fromEastNorth(fx.CHICAGO, 0, 350)), "aa", "300 km by default");
  assert.equal(borders.countryAt(atlantic, 300), "", "at sea");
});

test("borders: containment across the antimeridian and around a pole", () => {
  const fiji = {
    code: "fj",
    polygons: [
      [[{ lon: 175, lat: -20 }, { lon: 180, lat: -20 }, { lon: 180, lat: -15 }, { lon: 175, lat: -15 }]],
      [[{ lon: -180, lat: -20 }, { lon: -175, lat: -20 }, { lon: -175, lat: -15 }, { lon: -180, lat: -15 }]],
    ],
  };
  // closed along the pole, with a vertex every 5° along the coast, as Natural Earth's are dense
  const coast = Array.from({ length: 73 }, (_, i) => ({ lon: -180 + 5 * i, lat: -80 }));
  const antarctica = { code: "aq", polygons: [[[...coast, { lon: 180, lat: -90 }, { lon: -180, lat: -90 }]]] };
  const borders = new drift.Borders([fiji, antarctica]);
  assert.equal(borders.countryAt({ lat: -17, lon: 179.9 }, 0), "fj");
  assert.equal(borders.countryAt({ lat: -17, lon: -179.9 }, 0), "fj");
  assert.equal(borders.countryAt({ lat: -17, lon: 172 }, 0), "");
  // 3° of longitude west of Fiji at 17°S is about 320 km
  assert.equal(borders.countryAt({ lat: -17, lon: 172 }, 400), "fj");
  // and across the antimeridian from the other side
  assert.equal(borders.countryAt({ lat: -17, lon: -172 }, 400), "fj");
  assert.equal(borders.countryAt({ lat: -88, lon: 45 }, 0), "aq");
  assert.equal(borders.countryAt({ lat: -78, lon: 45 }, 300), "aq");
  assert.equal(borders.countryAt({ lat: -70, lon: 45 }, 300), "");
});

test("G2: the world map's data reads into borders, quantized or in degrees", () => {
  const borders = drift.readBorders({
    version: "ne-test",
    scale: 100,
    countries: [{ code: "AA", polygons: [[[100, 200, 300, 200, 300, 400, 100, 400]]] }],
  });
  assert.equal(borders.version, "ne-test");
  assert.deepEqual(borders.country("aa")!.polygons[0][0][1], { lon: 3, lat: 2 });
  assert.equal(borders.countryAt({ lat: 3, lon: 2 }, 0), "aa");
  const degrees = drift.readBorders({ countries: [{ code: "bb", polygons: [[[1, 2, 3, 2, 3, 4]]] }] });
  assert.equal(degrees.version, "");
  assert.deepEqual(degrees.country("bb")!.polygons[0][0][2], { lon: 3, lat: 4 });
  assert.throws(() => drift.readBorders({} as never), TypeError);
  assert.throws(() => drift.readBorders({ scale: 0, countries: [] }), TypeError);
});

test("G2: the world map loads once, lazily", async () => {
  const first = drift.loadDriftWorld();
  assert.equal(drift.loadDriftWorld(), first, "one load");
  const world = await first;
  assert.ok(world instanceof drift.Borders);
  if (world.codes().length === 0) {
    // the placeholder until connect's drift-borders writes the map
    return;
  }
  // T2's fixtures on the simplified map: Belgium, the Netherlands and Germany
  assert.equal(world.countryAt({ lat: 50.8503, lon: 4.3517 }, 0), "be", "Brussels");
  assert.equal(world.countryAt({ lat: 52.3676, lon: 4.9041 }, 0), "nl", "Amsterdam");
  assert.equal(world.countryAt({ lat: 50.9375, lon: 6.9603 }, 0), "de", "Cologne");
  assert.equal(world.countryAt(fx.CHICAGO, 0), "us");
  assert.equal(world.countryAt(fx.MADRID, 0), "es");
  assert.equal(world.countryAt(atlantic, 300), "", "mid-Atlantic");
  assert.ok(world.country("fr"));
});

// ---- W8

const frankfurtRun = (run: number, seed: number, extra: Partial<Parameters<typeof fx.syntheticRun>[1]> = {}) =>
  fx.syntheticRun(run, {
    truth: { lat: 50.105, lon: 8.66 },
    extenders: fx.EUROPE_EXTENDERS,
    lastMileMs: 12,
    queueMs: 2,
    count: 16,
    seed,
    ...extra,
  });

const europe = new drift.Borders([
  fx.squareCountry("es", fx.MADRID, 350),
  fx.squareCountry("de", fx.FRANKFURT, 250),
  fx.squareCountry("fr", { lat: 47, lon: 2.5 }, 250),
]);

test("W8: a run's observations are its extenders with 12 round trips, and it counts with 6 of them", () => {
  const run = frankfurtRun(1, 3);
  assert.equal(drift.driftRunObservations(run).length, 12);
  assert.equal(drift.driftRunCounts(run), true);
  const short = { ...run, extenders: run.extenders.map((e, i) => (i < 7 ? { ...e, roundTripsMs: e.roundTripsMs.slice(0, 11) } : e)) };
  assert.equal(drift.driftRunObservations(short).length, 5);
  assert.equal(drift.driftRunCounts(short), false);
  // lost pings are no round trips
  const lossy = { ...run, extenders: run.extenders.map((e) => ({ ...e, roundTripsMs: [...e.roundTripsMs.slice(0, 11), Number.NaN] })) };
  assert.equal(drift.driftRunObservations(lossy).length, 0);
  const o = drift.driftRunObservations(run)[0];
  assert.equal(o.run, 1);
  assert.equal(o.extenderId, "e0");
  assert.equal(o.roundTrip.count, 16);
  assert.deepEqual(o.roundTrip, drift.estimateRoundTrip(run.extenders[0].roundTripsMs));
});

test("W8: a visitor in their IP country is verified", () => {
  const result = drift.localDriftVerdict({
    borders: europe,
    ipCountryCode: "DE",
    expected: fx.FRANKFURT,
    runs: [frankfurtRun(1, 3)],
  });
  assert.equal(result.verdict, "agree");
  assert.equal(result.reason, "inside_ip_country");
  assert.deepEqual(result.runsInVerdict, [1]);
  assert.equal(result.ipCountryCode, "de");
  assert.equal(result.driftCountryCode, "de");
  assert.equal(result.distanceKm, 0);
  assert.ok(result.marginKm! < 0);
  assert.equal(result.confidence, 0.99);
  assert.ok(result.fit && result.judgement);
  close(result.plusMinusKm!, drift.regionRadiusKm(result.fit!, 0.99), 1e-9);
  assert.ok(drift.distanceKm(result.fit!.at, { lat: 50.105, lon: 8.66 }) < 100);
});

test("W8: pings that place the visitor clear of the IP country are rejected, with the figures the demo shows", () => {
  const result = drift.localDriftVerdict({
    borders: europe,
    ipCountryCode: "es",
    expected: fx.MADRID,
    runs: [frankfurtRun(1, 5, { truth: { lat: 50.105, lon: 8.66 } })],
  });
  assert.equal(result.verdict, "disagree");
  assert.equal(result.reason, "region_outside");
  assert.equal(result.driftCountryCode, "de");
  assert.ok(result.distanceKm! > 500, `distance ${result.distanceKm}`);
  assert.ok(result.marginKm! > 0);
  close(result.marginKm!, result.distanceKm! - result.significanceKm!, 1e-9);
  assert.equal(result.judgement!.overlaps, false);
});

test("W8: the verdict pools the last 3 counting runs", () => {
  const runs = [1, 2, 3, 4, 5].map((run) => frankfurtRun(run, run * 13));
  // run 4 has too few usable extenders and doesn't count
  runs[3] = { ...runs[3], extenders: runs[3].extenders.slice(0, 5) };
  const result = drift.localDriftVerdict({ borders: europe, ipCountryCode: "de", expected: fx.FRANKFURT, runs });
  assert.deepEqual(result.runsInVerdict, [2, 3, 5]);
  assert.deepEqual(result.fit!.runs, [2, 3, 5]);
  assert.equal(result.fit!.lastMileMs.length, 3);
  assert.equal(result.verdict, "agree");
  assert.equal(drift.driftWindowRuns, 3);
});

test("W8: inconclusive without a counting run, or without the IP country's borders", () => {
  const none = drift.localDriftVerdict({ borders: europe, ipCountryCode: "de", expected: fx.FRANKFURT, runs: [] });
  assert.equal(none.verdict, "inconclusive");
  assert.equal(none.reason, "too_few_extenders");
  assert.deepEqual(none.runsInVerdict, []);
  assert.equal(none.fit, null);
  const thin = frankfurtRun(1, 3);
  const few = drift.localDriftVerdict({
    borders: europe,
    ipCountryCode: "de",
    expected: fx.FRANKFURT,
    runs: [{ ...thin, extenders: thin.extenders.slice(0, 5) }],
  });
  assert.equal(few.reason, "too_few_extenders");
  // an IP country the map lacks: still the drift country and the ±, for testing accuracy
  const unknown = drift.localDriftVerdict({ borders: europe, ipCountryCode: "xk", expected: fx.FRANKFURT, runs: [thin] });
  assert.equal(unknown.verdict, "inconclusive");
  assert.equal(unknown.reason, "ip_country_unknown");
  assert.equal(unknown.driftCountryCode, "de");
  assert.ok(unknown.plusMinusKm! > 0);
  assert.equal(unknown.distanceKm, null);
});

test("W8: a geometry that can't place the visitor is inconclusive", () => {
  // every extender on one far line: σ_major beyond 1,500 km
  const line = Array.from({ length: 8 }, (_, i) => ({ lat: 10 + i * 0.01, lon: 100 + i * 0.01 }));
  const run = fx.syntheticRun(1, { truth: fx.FRANKFURT, extenders: line, lastMileMs: 10, queueMs: 2, count: 16, seed: 9 });
  const result = drift.localDriftVerdict({ borders: europe, ipCountryCode: "de", expected: fx.FRANKFURT, runs: [run] });
  assert.equal(result.verdict, "inconclusive");
  assert.equal(result.reason, "low_precision");
});
