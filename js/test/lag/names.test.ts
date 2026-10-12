import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const {
  canonicalIp,
  extenderMeasurementHost,
  extenderMeasurementLabel,
  parseExtenderMeasurementHost,
  trimSpaceGo,
  DEFAULT_EXTENDER_MEASUREMENT_ZONE,
} = await import("../../src/lag/names.ts");

// lag.dev/PLAN.md S1, S2, N: the canonical address, names and their parse,
// against what Go's netip and connect's lag_names.go give for the same input
const vectors = JSON.parse(readFileSync(new URL("./vectors.json", import.meta.url), "utf8"));

test("canonicalIp is netip.ParseAddr(...).Unmap().String() on every vector", () => {
  assert.ok(400 < vectors.canonicalIps.length);
  for (const { input, canonical } of vectors.canonicalIps) {
    assert.equal(canonicalIp(input), canonical, JSON.stringify(input));
  }
});

test("canonicalIp: RFC 5952, v4-mapped unmapped, zones kept, netip's refusals", () => {
  assert.equal(canonicalIp("2001:DB8:0:0:0:0:0:1"), "2001:db8::1");
  assert.equal(canonicalIp("2001:db8:0:0:1:0:0:1"), "2001:db8::1:0:0:1");
  assert.equal(canonicalIp("2001:db8:0:1:1:1:1:1"), "2001:db8:0:1:1:1:1:1");
  assert.equal(canonicalIp("::ffff:203.0.113.7"), "203.0.113.7");
  assert.equal(canonicalIp("::ffff:cb00:7107"), "203.0.113.7");
  assert.equal(canonicalIp("::1.2.3.4"), "::102:304");
  assert.equal(canonicalIp("fe80::1%eth0"), "fe80::1%eth0");
  assert.equal(canonicalIp("::ffff:1.2.3.4%eth0"), "1.2.3.4");
  for (const refused of ["01.2.3.4", "1.2.3", " 1.2.3.4", "1::2::3", "1:2:3:4:5:6:7:8::", "fe80::1%", "%eth0", ""]) {
    assert.equal(canonicalIp(refused), null, refused);
  }
});

test("extenderMeasurementHost is connect.ExtenderMeasurementName on every vector", () => {
  assert.ok(400 < vectors.measurementNames.length);
  for (const { ip, zone, name } of vectors.measurementNames) {
    assert.equal(extenderMeasurementHost(ip, zone), name, `${JSON.stringify(ip)} in ${JSON.stringify(zone)}`);
  }
});

test("the N1 table", () => {
  const table: Array<[string, string]> = [
    ["203.0.113.7", "203-0-113-7.extender.bringyour.com"],
    ["::ffff:203.0.113.7", "203-0-113-7.extender.bringyour.com"],
    ["2001:db8::1", "2001-db8--1.extender.bringyour.com"],
    ["2001:db8:0:1:1:1:1:1", "2001-db8-0-1-1-1-1-1.extender.bringyour.com"],
    ["2001:db8::", "2001-db8--0.extender.bringyour.com"],
    ["::1", "0--1.extender.bringyour.com"],
  ];
  for (const [ip, name] of table) {
    assert.equal(extenderMeasurementHost(ip), name);
    assert.equal(parseExtenderMeasurementHost(name), canonicalIp(ip));
  }
  assert.equal(DEFAULT_EXTENDER_MEASUREMENT_ZONE, "extender.bringyour.com");
});

test("names: the zone is normalized, the scope dropped, and a non-address has none", () => {
  assert.equal(extenderMeasurementHost("203.0.113.7", "Extender.BringYour.COM."), "203-0-113-7.extender.bringyour.com");
  assert.equal(extenderMeasurementHost("203.0.113.7", " \t"), "203-0-113-7.extender.bringyour.com");
  assert.equal(extenderMeasurementHost("203.0.113.7", "lab.example."), "203-0-113-7.lab.example");
  assert.equal(extenderMeasurementHost("fe80::1%eth0"), "fe80--1.extender.bringyour.com");
  assert.equal(extenderMeasurementLabel("::"), "0--0");
  assert.equal(extenderMeasurementHost("not an address"), "");
  assert.equal(extenderMeasurementHost(" 203.0.113.7"), "");
});

test("parseExtenderMeasurementHost is connect.ParseExtenderMeasurementName on every vector", () => {
  const hosts = vectors.measurementHosts as Array<{ host: string; zone: string; ip: string | null }>;
  assert.ok(400 < hosts.length);
  // the vectors hold refusals as well as round trips
  assert.ok(50 < hosts.filter((vector) => vector.ip === null).length);
  for (const { host, zone, ip } of hosts) {
    assert.equal(parseExtenderMeasurementHost(host, zone), ip, `${JSON.stringify(host)} in ${JSON.stringify(zone)}`);
  }
});

test("parseExtenderMeasurementHost refuses every spelling but the canonical one", () => {
  for (const refused of [
    "2001-DB8--1.extender.bringyour.com",
    "2001-db8-0-0-0-0-0-1.extender.bringyour.com",
    "2001-0db8--1.extender.bringyour.com",
    "--1.extender.bringyour.com",
    "00--1.extender.bringyour.com",
    "0--ffff-cb00-7107.extender.bringyour.com",
    "203-0-113-7.extender.bringyour.com.",
    "203-0-113-7.EXTENDER.BRINGYOUR.COM",
    "x.203-0-113-7.extender.bringyour.com",
    "203-0-113-7.example.com",
  ]) {
    assert.equal(parseExtenderMeasurementHost(refused), null, refused);
  }
  assert.equal(parseExtenderMeasurementHost("203-0-113-7.lab.example", "Lab.Example."), "203.0.113.7");
});

test("trimSpaceGo trims what Go's strings.TrimSpace trims", () => {
  assert.equal(trimSpaceGo("\u0085 1.2.3.4　\n"), "1.2.3.4");
  // U+FEFF is not space to Go, though String.prototype.trim removes it
  assert.equal(trimSpaceGo("﻿1.2.3.4"), "﻿1.2.3.4");
  assert.equal(trimSpaceGo(" \t\v\f\r\n"), "");
});
