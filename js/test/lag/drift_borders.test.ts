import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

// Borders and the drift country (lag.dev/DRIFTTEST.md V6, G2), the simplified
// world map (G2, I5) and the demo token's local verdict (W8, V7).

registerTsResolve();
const drift = await import("../../src/lag/drift/index.ts");
const { crc32 } = await import("../../src/lag/drift/bordersFormat.ts");
const fx = await import("./drift_fixtures.ts");

type Point = import("../../src/lag/drift/index.ts").Point;

const close = (actual: number, expected: number, tolerance: number, message?: string) =>
  assert.ok(Math.abs(actual - expected) <= tolerance, `${message ?? ""} ${actual} is not within ${tolerance} of ${expected}`);

const atlantic: Point = { lat: 40, lon: -40 };

test("borders: a country by code in either case, and every code, sorted", () => {
  const borders = new drift.Borders("test-1", [fx.squareCountry("bb", fx.MADRID, 100), fx.squareCountry("aa", fx.CHICAGO, 100)]);
  assert.equal(borders.version(), "test-1");
  assert.deepEqual(borders.codes(), ["aa", "bb"]);
  assert.equal(borders.country("aa")?.code, "aa");
  assert.equal(borders.country("AA")?.code, "aa");
  assert.equal(borders.country("cc"), null);
  // a code listed twice is one country with both polygons
  const twice = new drift.Borders("", [fx.squareCountry("aa", fx.CHICAGO, 100), fx.squareCountry("aa", fx.MADRID, 100)]);
  assert.equal(twice.country("aa")!.polygons.length, 2);
  assert.equal(twice.countryAt(fx.MADRID, 0), "aa");
  // GeoLite2's codes: two lower-case letters
  for (const code of ["AA", "a", "abc", "a1", ""]) {
    assert.throws(() => new drift.Borders("", [fx.squareCountry(code, fx.MADRID, 100)]), TypeError, code);
  }
});

test("V6: the drift country is the one containing the fit, else the nearest within 300 km, else none", () => {
  const borders = new drift.Borders("", [
    fx.squareCountry("aa", fx.CHICAGO, 100),
    fx.squareCountry("bb", drift.fromAzimuthalKm(fx.CHICAGO, 500, 0), 100),
  ]);
  assert.equal(drift.driftCountryWithinKm, 300);
  assert.equal(borders.countryAt(fx.CHICAGO, 300), "aa");
  assert.equal(borders.countryAt(drift.fromAzimuthalKm(fx.CHICAGO, 500, 50), 300), "bb");
  // 150 km east of aa's border and 150 km west of bb's: the nearer by a little
  assert.equal(borders.countryAt(drift.fromAzimuthalKm(fx.CHICAGO, 240, 0), 300), "aa");
  assert.equal(borders.countryAt(drift.fromAzimuthalKm(fx.CHICAGO, 260, 0), 300), "bb");
  // 250 km north of aa: within 300 km, not within 200
  assert.equal(borders.countryAt(drift.fromAzimuthalKm(fx.CHICAGO, 0, 350), 300), "aa");
  assert.equal(borders.countryAt(drift.fromAzimuthalKm(fx.CHICAGO, 0, 350), 200), "");
  assert.equal(borders.countryAt(drift.fromAzimuthalKm(fx.CHICAGO, 0, 350)), "aa", "300 km by default");
  assert.equal(borders.countryAt(atlantic, 300), "", "at sea");
  assert.equal(borders.countryAt({ lat: 91, lon: 0 }, 300), "", "not a place");
  // overlapping countries, as a disputed area given to each claimant: the first code wins
  const disputed = new drift.Borders("", [fx.squareCountry("zz", fx.MADRID, 100), fx.squareCountry("yy", fx.MADRID, 50)]);
  assert.equal(disputed.countryAt(fx.MADRID, 0), "yy");
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
  const borders = new drift.Borders("", [fiji, antarctica]);
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

test("G2: the world map reads as lagdrift's ReadWorld reads it", () => {
  const data = {
    format: "lagdrift-world/1",
    version: "ne-test",
    simplifyKm: 5,
    scale: 100,
    countries: [{ code: "aa", polygons: [[[100, 200, 300, 200, 300, 400, 100, 400]]] }],
  };
  const borders = drift.readWorld(data);
  assert.equal(drift.worldFormat, "lagdrift-world/1");
  assert.equal(borders.version(), "ne-test");
  assert.deepEqual(borders.country("aa")!.polygons[0][0][1], { lon: 3, lat: 2 });
  assert.equal(borders.countryAt({ lat: 3, lon: 2 }, 0), "aa");
  assert.deepEqual(drift.readWorld(JSON.stringify(data)).codes(), ["aa"], "or its text");
  const broken = (change: Record<string, unknown>) => () => drift.readWorld({ ...data, ...change } as never);
  assert.throws(broken({ format: "lagdrift-world/2" }), TypeError);
  assert.throws(broken({ scale: 0 }), TypeError);
  assert.throws(broken({ countries: [{ code: "aa", polygons: [[[1, 2, 3, 4]]] }] }), TypeError, "a ring of 2 points");
  assert.throws(broken({ countries: [{ code: "aa", polygons: [[[1, 2, 3, 4, 5]]] }] }), TypeError, "an odd ring");
  assert.throws(broken({ countries: [{ code: "aa", polygons: [[]] }] }), TypeError, "a polygon with no rings");
  assert.throws(broken({ countries: [{ code: "aa", polygons: [[[0, 0, 100, 0, 100, 9100]]] }] }), TypeError, "a point past the pole");
  assert.throws(broken({ countries: [{ code: "AA", polygons: [[[100, 200, 300, 200, 300, 400]]] }] }), TypeError, "an upper-case code");
});

// lagdrift's borders format (G1, I5), written here for the reader's tests
function writeBordersFile(version: string, countries: { code: string; polygons: [number, number][][][] }[]): Uint8Array {
  const out: number[] = [];
  const uvarint = (v: number) => {
    while (0x80 <= v) {
      out.push((v % 0x80) | 0x80);
      v = Math.floor(v / 0x80);
    }
    out.push(v);
  };
  const varint = (v: number) => uvarint(v < 0 ? -2 * v - 1 : 2 * v);
  out.push(...new TextEncoder().encode("lagdrift-borders"));
  uvarint(1);
  const versionBytes = new TextEncoder().encode(version);
  uvarint(versionBytes.length);
  out.push(...versionBytes);
  uvarint(countries.length);
  for (const country of countries) {
    out.push(...new TextEncoder().encode(country.code));
    const all = country.polygons.flat(2);
    for (const v of [
      Math.min(...all.map(([lat]) => lat)),
      Math.min(...all.map(([, lon]) => lon)),
      Math.max(...all.map(([lat]) => lat)),
      Math.max(...all.map(([, lon]) => lon)),
    ]) {
      varint(v);
    }
    uvarint(country.polygons.length);
    for (const polygon of country.polygons) {
      uvarint(polygon.length);
      for (const ring of polygon) {
        uvarint(ring.length);
        let lat = 0;
        let lon = 0;
        for (const [pLat, pLon] of ring) {
          varint(pLat - lat);
          varint(pLon - lon);
          lat = pLat;
          lon = pLon;
        }
      }
    }
  }
  const body = Uint8Array.from(out);
  const file = new Uint8Array(body.length + 4);
  file.set(body);
  new DataView(file.buffer).setUint32(body.length, crc32(body), true);
  return file;
}

test("G1: the borders file reads as lagdrift's ReadBorders reads it, and a damaged one is refused", () => {
  const square: [number, number][] = [
    [40_000_000, -4_000_000],
    [40_000_000, -3_000_000],
    [41_000_000, -3_000_000],
    [41_000_000, -4_000_000],
  ];
  const hole: [number, number][] = [
    [40_400_000, -3_600_000],
    [40_400_000, -3_400_000],
    [40_600_000, -3_400_000],
  ];
  const file = writeBordersFile("ne-admin0-10m-test", [
    { code: "ad", polygons: [[[[42_500_000, 1_500_000], [42_600_000, 1_600_000], [42_500_000, 1_700_000]]]] },
    { code: "es", polygons: [[square, hole]] },
  ]);
  // CRC-32 (IEEE), as Go's crc32.ChecksumIEEE: the standard check value
  assert.equal(crc32(new TextEncoder().encode("123456789")), 0xcbf43926);
  const borders = drift.readBorders(file);
  assert.equal(borders.version(), "ne-admin0-10m-test");
  assert.deepEqual(borders.codes(), ["ad", "es"]);
  assert.deepEqual(borders.country("es")!.polygons[0][0][1], { lat: 40, lon: -3 });
  assert.equal(borders.countryAt({ lat: 40.2, lon: -3.2 }, 0), "es");
  assert.equal(borders.countryAt({ lat: 40.45, lon: -3.45 }, 0), "", "in the hole");
  // every byte counts
  const flipped = file.slice();
  flipped[30] ^= 1;
  assert.throws(() => drift.readBorders(flipped), TypeError, "the checksum");
  assert.throws(() => drift.readBorders(file.subarray(0, file.length - 1)), TypeError, "truncated");
  assert.throws(() => drift.readBorders(new TextEncoder().encode("not-a-borders-file")), TypeError);
  const unordered = writeBordersFile("v", [
    { code: "es", polygons: [[square]] },
    { code: "ad", polygons: [[square]] },
  ]);
  assert.throws(() => drift.readBorders(unordered), TypeError, "out of order");
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
  // Natural Earth 1:50m, every code GeoLite2 gives a country
  assert.match(world.version(), /^ne-admin0-50m-v/);
  assert.ok(world.codes().length >= 240, `${world.codes().length} countries`);
  // across the antimeridian, at a pole, and off a coast
  assert.equal(world.countryAt({ lat: -17.8, lon: 178.0 }, 0), "fj", "Viti Levu");
  assert.equal(world.countryAt({ lat: 66.0, lon: -172.0 }, 0), "ru", "Chukotka, west of the antimeridian");
  assert.equal(world.countryAt({ lat: -85, lon: 0 }, 0), "aq");
  assert.equal(world.countryAt({ lat: 38.7, lon: -10.5 }, 0), "", "at sea off Lisbon");
  assert.equal(world.countryAt({ lat: 38.7, lon: -10.5 }, 300), "pt", "the nearest within 300 km");
});

test("W8: the demo's verdict against the simplified map", async () => {
  const world = await drift.loadDriftWorld();
  if (world.codes().length === 0) {
    return;
  }
  const run = (truth: { lat: number; lon: number }, seed: number) =>
    fx.syntheticRun(1, { truth, extenders: fx.EUROPE_EXTENDERS, lastMileMs: 10, queueMs: 2, count: 16, seed });
  const frankfurt = { lat: 50.105, lon: 8.66 };
  const home = drift.localDriftVerdict({ borders: world, ipCountryCode: "de", expected: fx.FRANKFURT, runs: [run(frankfurt, 3)] });
  assert.equal(home.verdict, "agree");
  assert.equal(home.driftCountryCode, "de");
  // pings from Frankfurt on an address that says Spain: rejected, the pings pointing to Germany
  const away = drift.localDriftVerdict({ borders: world, ipCountryCode: "es", expected: fx.MADRID, runs: [run(frankfurt, 5)] });
  assert.equal(away.verdict, "disagree");
  assert.equal(away.reason, "region_outside");
  assert.equal(away.driftCountryCode, "de");
  assert.ok(away.marginKm! > 0 && away.distanceKm! > 300, `distance ${away.distanceKm}, margin ${away.marginKm}`);
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

const europe = new drift.Borders("test", [
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
