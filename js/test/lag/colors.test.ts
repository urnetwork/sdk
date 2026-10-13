import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const {
  countryColorHex,
  extenderColorHex,
  extenderHue,
  extenderHueColorHex,
  UNKNOWN_COUNTRY_COLOR_HEX,
} = await import("../../src/lag/colors.ts");
const { COUNTRY_COLOR_HEXES } = await import("../../src/lag/country_colors.generated.ts");

// lag.dev/PLAN.md S1, S2: colors as the sdk's GetColorHex and
// GetExtenderColorHex give them
const vectors = JSON.parse(readFileSync(new URL("./vectors.json", import.meta.url), "utf8"));
const countryColors = JSON.parse(
  readFileSync(new URL("../../src/lag/country_colors.generated.json", import.meta.url), "utf8"),
) as Record<string, string>;

test("extenderColorHex and extenderHue are GetExtenderColorHex on every vector", () => {
  assert.ok(600 < vectors.extenderColors.length);
  for (const { ip, hue, colorHex } of vectors.extenderColors) {
    assert.equal(extenderHue(ip), hue, JSON.stringify(ip));
    assert.equal(extenderColorHex(ip), colorHex, JSON.stringify(ip));
  }
});

test("every one of the 360 hues is Go's color", () => {
  assert.equal(vectors.hueColors.length, 360);
  vectors.hueColors.forEach((colorHex: string, hue: number) => {
    assert.match(colorHex, /^[0-9a-f]{6}$/);
    assert.equal(extenderHueColorHex(hue), colorHex, `hue ${hue}`);
  });
  for (const outside of [-1, 360, 1.5, Number.NaN]) {
    assert.throws(() => extenderHueColorHex(outside), RangeError);
  }
});

test("an address written two ways is one color", () => {
  assert.equal(extenderColorHex("::ffff:203.0.113.7"), extenderColorHex("203.0.113.7"));
  assert.equal(extenderColorHex(" 2001:DB8:0::1\n"), extenderColorHex("2001:db8::1"));
});

test("the generated table and its json are the same 250 codes", () => {
  const codes = Object.keys(COUNTRY_COLOR_HEXES);
  assert.equal(codes.length, 250);
  assert.deepEqual(COUNTRY_COLOR_HEXES, countryColors);
  for (const code of ["de", "us", "jp", "xk", "aq", "um"]) {
    assert.ok(code in countryColors, code);
  }
  for (const [code, colorHex] of Object.entries(countryColors)) {
    assert.match(code, /^[a-z]{2}$/);
    assert.match(colorHex, /^[0-9a-fA-F]{6}$/);
  }
});

test("countryColorHex reads the table without case or space, and a neutral color otherwise", () => {
  assert.equal(countryColorHex("de"), "663F46");
  assert.equal(countryColorHex("DE"), "663F46");
  assert.equal(countryColorHex(" de\n"), "663F46");
  for (const [code, colorHex] of Object.entries(countryColors)) {
    assert.equal(countryColorHex(code), colorHex);
  }
  for (const unknown of ["", "zz", "eu", "deu", "constructor", "__proto__", "toString"]) {
    assert.equal(countryColorHex(unknown), UNKNOWN_COUNTRY_COLOR_HEX, unknown);
  }
});
