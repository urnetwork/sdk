import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";

// A country's continent for V11's calibrated k (lag.dev/DRIFTTEST.md V11),
// pinned to the server's model.ContinentCodeForCountry by vectors it wrote
// over every two-letter code, then case, space and unknown codes.

registerTsResolve();
const drift = await import("../../src/lag/drift/index.ts");

const vectors = JSON.parse(readFileSync(new URL("./drift-continents.json", import.meta.url), "utf8")) as {
  continents: string[];
  vectors: { country: string; continent: string }[];
};

test("V11: every country's continent is the server's", () => {
  assert.deepEqual(drift.continentCodes, vectors.continents);
  assert.equal(vectors.vectors.length, 706);
  for (const { country, continent } of vectors.vectors) {
    assert.equal(drift.continentForCountry(country), continent, JSON.stringify(country));
  }
});

test("V11: a country's continent in either case, trimmed, and none for an unknown code", () => {
  assert.equal(drift.continentForCountry("US"), "NA");
  assert.equal(drift.continentForCountry(" nz "), "OC");
  assert.equal(drift.continentForCountry(" de　"), "EU", "Go's TrimSpace trims Unicode space");
  assert.equal(drift.continentForCountry("aq"), "AN");
  assert.equal(drift.continentForCountry("xk"), "EU");
  assert.equal(drift.continentForCountry(""), "");
  assert.equal(drift.continentForCountry("zz"), "");
  assert.equal(drift.continentForCountry("constructor"), "", "no object property is a country");
  // it feeds defaultK directly: an unknown country is the unknown continent
  assert.equal(drift.defaultK(0.99, drift.continentForCountry("nz")), 7.88);
  assert.equal(drift.defaultK(0.99, drift.continentForCountry("fr")), drift.kForConfidence(0.99));
  assert.equal(drift.defaultK(0.99, drift.continentForCountry("zz")), 3.49);
});
