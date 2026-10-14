import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const {
  canonicalIp,
  extenderIdFromBytes,
  extenderMeasurementHost,
  isExtenderId,
  parseExtenderMeasurementHost,
  trimSpaceGo,
  DEFAULT_EXTENDER_MEASUREMENT_ZONE,
} = await import("../../src/lag/names.ts");

// lag.dev/PLAN.md S1, S2, N: the canonical address, and measurement names by
// extender id and their parse, against what Go's netip and connect's
// lag_names.go give for the same input
const vectors = JSON.parse(readFileSync(new URL("./vectors.json", import.meta.url), "utf8"));

const exampleId = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f";
const exampleHost = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com";

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

test("isExtenderId takes only what connect.Id.String() writes, on every vector", () => {
  const ids = vectors.extenderIds as Array<{ text: string; canonical: boolean }>;
  assert.ok(30 <= ids.length);
  assert.ok(ids.some((vector) => vector.canonical) && ids.some((vector) => !vector.canonical));
  for (const { text, canonical } of ids) {
    assert.equal(isExtenderId(text), canonical, JSON.stringify(text));
    // an id that is not canonical names no host
    assert.equal(extenderMeasurementHost(text), canonical ? `${text}.extender.bringyour.com` : "", JSON.stringify(text));
  }
});

test("extenderMeasurementHost is connect.ExtenderMeasurementName on every vector", () => {
  const names = vectors.measurementNames as Array<{ id: string; zone: string; name: string }>;
  assert.ok(60 <= names.length);
  for (const { id, zone, name } of names) {
    assert.equal(extenderMeasurementHost(id, zone), name, `${id} in ${JSON.stringify(zone)}`);
  }
});

test("the N1 example", () => {
  assert.equal(DEFAULT_EXTENDER_MEASUREMENT_ZONE, "extender.bringyour.com");
  assert.equal(extenderMeasurementHost(exampleId), exampleHost);
  assert.equal(parseExtenderMeasurementHost(exampleHost), exampleId);
});

test("names: the zone is normalized and a non-canonical id is refused", () => {
  assert.equal(extenderMeasurementHost(exampleId, "Extender.BringYour.COM."), exampleHost);
  assert.equal(extenderMeasurementHost(exampleId, " \t"), exampleHost);
  assert.equal(extenderMeasurementHost(exampleId, ""), exampleHost);
  assert.equal(extenderMeasurementHost(exampleId, "  Lab.Example. "), `${exampleId}.lab.example`);
  for (const refused of [
    exampleId.toUpperCase(),
    exampleId.replace(/-/g, ""),
    `{${exampleId}}`,
    exampleId.slice(0, -1),
    `${exampleId}0`,
    exampleId.replace("0192", "g192"),
    ` ${exampleId}`,
    `${exampleId}\n`,
    "",
    "203.0.113.7",
    "203-0-113-7",
  ]) {
    assert.equal(extenderMeasurementHost(refused), "", JSON.stringify(refused));
  }
});

test("parseExtenderMeasurementHost is connect.ParseExtenderMeasurementName on every vector", () => {
  const hosts = vectors.measurementHosts as Array<{ host: string; zone: string; id: string | null }>;
  assert.ok(100 < hosts.length);
  // the vectors hold refusals as well as round trips
  assert.ok(40 < hosts.filter((vector) => vector.id === null).length);
  assert.ok(60 <= hosts.filter((vector) => vector.id !== null).length);
  for (const { host, zone, id } of hosts) {
    assert.equal(parseExtenderMeasurementHost(host, zone), id, `${JSON.stringify(host)} in ${JSON.stringify(zone)}`);
  }
});

test("parseExtenderMeasurementHost refuses every spelling but the canonical one", () => {
  for (const refused of [
    exampleHost.toUpperCase(),
    `${exampleId.toUpperCase()}.extender.bringyour.com`,
    `${exampleId.replace(/-/g, "")}.extender.bringyour.com`,
    `${exampleId.replace(/-/g, "x")}.extender.bringyour.com`,
    `${exampleId.slice(1)}.extender.bringyour.com`,
    `${exampleId}0.extender.bringyour.com`,
    `${exampleId.replace("0192", "g192")}.extender.bringyour.com`,
    `${exampleId}.EXTENDER.BRINGYOUR.COM`,
    `${exampleHost}.`,
    `x.${exampleHost}`,
    `${exampleId}.x.extender.bringyour.com`,
    `${exampleId}.example.com`,
    ` ${exampleHost}`,
    exampleId,
    ".extender.bringyour.com",
    "203-0-113-7.extender.bringyour.com",
    "",
  ]) {
    assert.equal(parseExtenderMeasurementHost(refused), null, JSON.stringify(refused));
  }
  assert.equal(parseExtenderMeasurementHost(`${exampleId}.lab.example`, "Lab.Example."), exampleId);
  assert.equal(parseExtenderMeasurementHost(exampleHost, "lab.example"), null);
});

test("extenderIdFromBytes writes 16 bytes as connect.Id.String() does", () => {
  const bytes = Uint8Array.from(Buffer.from(exampleId.replace(/-/g, ""), "hex"));
  assert.equal(extenderIdFromBytes(bytes), exampleId);
  assert.equal(extenderIdFromBytes(new Uint8Array(16).fill(0xff)), "ffffffff-ffff-ffff-ffff-ffffffffffff");
  for (const length of [0, 8, 15, 17, 32]) {
    assert.equal(extenderIdFromBytes(new Uint8Array(length).fill(1)), null, `${length} bytes`);
  }
  // every id the vectors name round trips through its bytes; the zero id is
  // no extender's, and Go names it nothing
  for (const { id, name } of vectors.measurementNames as Array<{ id: string; name: string }>) {
    const named = name !== "";
    assert.equal(extenderIdFromBytes(Uint8Array.from(Buffer.from(id.replace(/-/g, ""), "hex"))), named ? id : null, id);
    assert.equal(isExtenderId(id), named, id);
  }
});

test("the zero id is no extender's: it has no host and does not parse", () => {
  const zero = "00000000-0000-0000-0000-000000000000";
  assert.equal(isExtenderId(zero), false);
  assert.equal(extenderIdFromBytes(new Uint8Array(16)), null);
  assert.equal(extenderMeasurementHost(zero), "");
  assert.equal(parseExtenderMeasurementHost(`${zero}.extender.bringyour.com`), null);
  assert.equal(isExtenderId("00000000-0000-0000-0000-000000000001"), true);
});

test("trimSpaceGo trims what Go's strings.TrimSpace trims", () => {
  assert.equal(trimSpaceGo("\u0085 1.2.3.4\u3000\n"), "1.2.3.4");
  // U+FEFF is not space to Go, though String.prototype.trim removes it
  assert.equal(trimSpaceGo("\ufeff1.2.3.4"), "\ufeff1.2.3.4");
  assert.equal(trimSpaceGo(" \t\v\f\r\n"), "");
});
