import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "../../test/ts-resolve.ts";
import { PLACES } from "./mock_api.ts";

registerTsResolve();
const { rpmBand, rpmFor, rpmOf } = await import("../src/rpm.ts");
const { selectServers } = await import("../src/select.ts");
const { guestFileName } = await import("../src/guest_store.ts");
const { parseHostPort } = await import("../src/net.ts");
const { formatMeasurement, megabytes } = await import("../src/format.ts");
const { VERSION } = await import("../src/version.ts");
const { parseBytes } = await import("../src/cli.ts");

test("RPM is 60,000 over the round trip, banded as draft-ietf-ippm-responsiveness-09", () => {
  assert.equal(rpmOf(60), 1000);
  assert.equal(rpmBand(299.9), "poor");
  assert.equal(rpmBand(rpmOf(200)), "fair"); // 300 exactly
  assert.equal(rpmBand(999), "fair");
  assert.equal(rpmBand(1000), "good");
  assert.equal(rpmBand(5999), "good");
  assert.equal(rpmBand(rpmOf(10)), "excellent"); // 6000 exactly
  assert.deepEqual(rpmFor(12), { value: 5000, band: "good" });
  assert.equal(rpmFor(null), null);
  assert.equal(rpmFor(0), null);
  assert.throws(() => rpmOf(-1), RangeError);
});

const extender = (id: string, place: keyof typeof PLACES) => ({
  extender_id: id,
  public_key: "00",
  host: `${id}.extender.bringyour.com`,
  ips: [],
  location: PLACES[place],
});

const AUSTIN = { latitude: 30.2672, longitude: -97.7431, countryCode: "us" };

test("selection: 3 near in distinct cities, then far ones in new countries, nearest first", () => {
  const extenders = Object.keys(PLACES).map((place, i) => extender(`e${i}`, place as keyof typeof PLACES));
  // a second Houston extender: near picks prefer distinct cities
  extenders.push(extender("e-houston-2", "houston"));
  const { servers, spares } = selectServers(AUSTIN, extenders, { count: 8 });
  assert.equal(servers.length, 8);
  assert.deepEqual(servers.slice(0, 3).map((s) => s.city), ["Houston", "Dallas", "Mexico City"]);
  for (let i = 1; i < servers.length; i += 1) {
    assert.ok(servers[i - 1].distanceKm <= servers[i].distanceKm, "nearest first");
  }
  const far = servers.slice(3);
  assert.ok(far.every((s) => s.countryCode !== "us"), "far picks prefer countries other than the visitor's");
  assert.equal(new Set(far.map((s) => s.countryCode)).size, far.length, "one per country while new ones remain");
  assert.ok(far.every((s) => s.distanceKm >= 1500));
  const taken = new Set(servers.map((s) => s.extenderId));
  assert.ok(spares.every((s) => !taken.has(s.extenderId)));
  assert.ok(spares.length > 0 && spares.length <= 4);
});

test("selection: one per extender, fewer servers when fewer exist, and spread alone without a place", () => {
  const two = [extender("a", "tokyo"), extender("a", "tokyo"), extender("b", "london")];
  assert.equal(selectServers(AUSTIN, two, { count: 8 }).servers.length, 2);
  const unplaced = selectServers({}, Object.keys(PLACES).map((p, i) => extender(`e${i}`, p as keyof typeof PLACES)), { count: 4 });
  assert.equal(unplaced.servers.length, 4);
  assert.ok(unplaced.servers.every((s) => Number.isNaN(s.distanceKm)));
  assert.equal(new Set(unplaced.servers.map((s) => s.countryCode)).size, 4);
  assert.deepEqual(selectServers(AUSTIN, [], {}), { servers: [], spares: [] });
});

test("sizes, addresses and file names", () => {
  assert.equal(parseBytes("500MB"), 500e6);
  assert.equal(parseBytes("2 GB"), 2e9);
  assert.equal(parseBytes("1GiB"), 2 ** 30);
  assert.equal(parseBytes("12"), 12);
  assert.throws(() => parseBytes("lots"));
  assert.throws(() => parseBytes("0MB"));
  assert.deepEqual(parseHostPort("127.0.0.1:47443"), { host: "127.0.0.1", port: 47443 });
  assert.deepEqual(parseHostPort("[::1]:8443"), { host: "::1", port: 8443 });
  assert.throws(() => parseHostPort("127.0.0.1"));
  assert.equal(guestFileName("https://api.bringyour.com"), "guest-api.bringyour.com.json");
  assert.equal(
    guestFileName("https://api.bringyour.com", { host: "127.0.0.1", port: 47443 }),
    "guest-api.bringyour.com@127.0.0.1-47443.json",
  );
  assert.equal(megabytes(612_400_000), "612 MB");
});

test("the version matches package.json", () => {
  const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as { version: string };
  assert.equal(VERSION, pkg.version);
});

test("the text output names each server and the overall figures", () => {
  const text = formatMeasurement({
    tool: "lagdev",
    version: VERSION,
    mode: "latency",
    startedAt: new Date(0).toISOString(),
    durationMs: 15_200,
    api: "https://api.bringyour.com",
    visitor: { city: "Austin", region: "Texas", countryCode: "us", asn: { number: 64500, organization: "Example" } },
    servers: [
      {
        extenderId: "a",
        city: "Houston",
        country: "United States",
        countryCode: "us",
        distanceKm: 235,
        pings: 60,
        lost: 0,
        idle: { p50Ms: 12.3, p95Ms: 15 },
        rpm: { value: 4878, band: "good" },
      },
      {
        extenderId: "bbbbbbbb-0000",
        city: "",
        country: "",
        countryCode: "",
        distanceKm: null,
        pings: 0,
        lost: 0,
        idle: null,
        rpm: null,
        error: "connect",
      },
    ],
    overall: {
      pings: 60,
      lost: 0,
      idle: { p50Ms: 12.3, p95Ms: 15 },
      best: { extenderId: "a", city: "Houston", country: "United States", idleP50Ms: 12.3 },
      rpm: { value: 4878, band: "good" },
    },
    bytes: { download: 0, upload: 0, total: 0 },
    stoppedEarly: null,
    notes: [],
  });
  assert.match(text, /lag\.dev latency · from Austin, Texas, US · AS64500 Example · 1 servers · 15 s/);
  assert.match(text, /Houston, US\s+235\s+12\.3\s+15\.0\s+0\s+60\s+4,878 good/);
  assert.match(text, /bbbbbbbb.*not measured: connect/);
  assert.match(text, /best: Houston at 12\.3 ms · idle RPM 4,878 good/);
});
