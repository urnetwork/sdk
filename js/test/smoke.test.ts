import {test} from "node:test";
import assert from "node:assert/strict";
import {existsSync} from "node:fs";
import {initWasm, isWasmInitialized, getWasmGlobals} from "../src/loader.ts";

test("JS SDK smoke loads the WASM runtime and closes it", {
  skip: !existsSync(new URL("../wasm/sdk.wasm", import.meta.url)),
  timeout: 60000,
}, async () => {
  await initWasm();
  assert.equal(isWasmInitialized(), true);
  const exports = getWasmGlobals();
  assert.equal(typeof exports.URnetworkNewPlatformDeviceRemote, "function");
  assert.equal(typeof exports.URnetworkClose, "function");
  // the embedded license list, led by the GeoLite2 notice every app must show
  assert.equal(typeof exports.URnetworkGetLicenses, "function");
  const web = exports.URnetworkGetLicenses("web");
  assert.ok(Array.isArray(web) && web.length > 0);
  assert.equal(web[0].name, "GeoLite2 by MaxMind");
  assert.match(web[0].notice, /This product includes GeoLite2 data created by MaxMind/);
  assert.ok(web.every((license: {text: string}) => license.text.length > 0));
  assert.ok(exports.URnetworkGetLicenses("").length > web.length);
  // the sdk's location grouping over a raw find-provider-locations result
  assert.equal(typeof exports.URnetworkFilteredLocationsFromResult, "function");
  const found = JSON.stringify({
    specs: [],
    groups: [{location_group_id: "0192a0b0-0000-7000-8000-000000000001", name: "Strong Privacy", provider_count: 5, promoted: true, match_distance: 3}],
    locations: [
      {location_id: "0192a0b0-0000-7000-8000-000000000002", location_type: "country", name: "Germany", country_code: "de", provider_count: 9, match_distance: 0},
      {location_id: "0192a0b0-0000-7000-8000-000000000003", location_type: "region", name: "Bavaria", country_code: "de", provider_count: 4, match_distance: 2},
      {location_id: "0192a0b0-0000-7000-8000-000000000004", location_type: "city", name: "Munich", country_code: "de", provider_count: 3, match_distance: 2,
        region_location_id: "0192a0b0-0000-7000-8000-000000000003"},
      {location_id: "0192a0b0-0000-7000-8000-000000000005", location_type: "country", name: "Austria", country_code: "at", provider_count: 12, match_distance: 4},
      {location_id: "0192a0b0-0000-7000-8000-000000000006", location_type: "country", name: "France", country_code: "fr", provider_count: 15, match_distance: 5},
    ],
    devices: [],
  });
  const browse = exports.URnetworkFilteredLocationsFromResult(found, "");
  assert.deepEqual(Object.keys(browse).sort(), ["bestMatches", "cities", "countries", "devices", "promoted", "regionGroups", "regions"]);
  assert.deepEqual(browse.bestMatches, []);
  assert.deepEqual(browse.promoted.map((l: {name: string}) => l.name), ["Strong Privacy"]);
  // close matches (distance <= 1) first, then provider count descending
  assert.deepEqual(browse.countries.map((l: {name: string}) => l.name), ["Germany", "France", "Austria"]);
  assert.equal(browse.countries[0].locationType, "country");
  assert.equal(browse.countries[0].countryCode, "de");
  assert.equal(browse.countries[0].providerCount, 9);
  assert.equal(browse.countries[0].locationId, "0192a0b0-0000-7000-8000-000000000002");
  assert.equal(typeof browse.countries[0].connectLocationId, "string");
  assert.equal(typeof browse.countries[0].colorHex, "string");
  assert.deepEqual(browse.regionGroups, []);
  const search = exports.URnetworkFilteredLocationsFromResult(found, "germany");
  assert.deepEqual(search.bestMatches.map((l: {name: string}) => l.name), ["Germany"]);
  assert.deepEqual(search.regions.map((l: {name: string}) => l.name), ["Bavaria"]);
  assert.deepEqual(search.cities.map((l: {name: string}) => l.name), ["Munich"]);
  assert.equal(search.regionGroups.length, 1);
  assert.equal(search.regionGroups[0].region.name, "Bavaria");
  assert.deepEqual(search.regionGroups[0].cities.map((l: {name: string}) => l.name), ["Munich"]);
  assert.equal(exports.URnetworkFilteredLocationsFromResult("not json", ""), null);
  exports.URnetworkClose();
  assert.equal(isWasmInitialized(), false);
});
