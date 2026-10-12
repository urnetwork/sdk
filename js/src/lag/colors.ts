// Country and extender colors, as the apps draw them (lag.dev/PLAN.md S1, S2;
// connect/EXTENDER.md K3).
//
// A country's color is the sdk palette's (sdk GetColorHex), read from a table
// generated from Go (country_colors.generated.ts). An extender's ring color is
// computed as sdk GetExtenderColorHex computes it: FNV-1a 32 over the canonical
// address text, hue the hash modulo 360, saturation 70 percent, lightness 55
// percent. The arithmetic is the same IEEE double arithmetic in the same order,
// so every hue matches Go to the bit; the vectors pin all 360.

import { COUNTRY_COLOR_HEXES } from "./country_colors.generated";
import { canonicalIp, trimSpaceGo } from "./names";

/**
 * The color of a country code the table does not hold. Every ISO 3166-1
 * alpha-2 code is in the table, and XK, so this is for an empty or unknown
 * code: a neutral gray rather than the palette mix Go derives for any string.
 */
export const UNKNOWN_COUNTRY_COLOR_HEX = "9e9e9e";

/**
 * The palette color of a country (six hex digits, no "#"), as sdk
 * `GetColorHex` gives it for the lower case code, so a country is the same
 * color here as in the apps. The code is matched without case or surrounding
 * space. A code outside ISO 3166-1 alpha-2 (and XK) gets
 * `UNKNOWN_COUNTRY_COLOR_HEX`.
 *
 * @example
 * countryColorHex("de") // "663F46"
 */
export function countryColorHex(code: string): string {
  const key = code.trim().toLowerCase();
  return Object.prototype.hasOwnProperty.call(COUNTRY_COLOR_HEXES, key)
    ? COUNTRY_COLOR_HEXES[key]
    : UNKNOWN_COUNTRY_COLOR_HEX;
}

// FNV-1a 32 over bytes
function fnv1a32(bytes: Uint8Array): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < bytes.length; i += 1) {
    hash = Math.imul(hash ^ bytes[i], 0x01000193) >>> 0;
  }
  return hash;
}

const utf8 = new TextEncoder();

/**
 * The hue (0 to 359) of an extender address's ring color: FNV-1a 32 of its
 * canonical text, modulo 360. This is the "ring hue" a share payload carries
 * (L10). As in Go, surrounding space is ignored and text that is not an
 * address is hashed as it is.
 */
export function extenderHue(ip: string): number {
  const name = trimSpaceGo(ip);
  return fnv1a32(utf8.encode(canonicalIp(name) ?? name)) % 360;
}

function hex2(value: number): string {
  return value.toString(16).padStart(2, "0");
}

// sdk hslColorHex: the standard hsl to rgb conversion, hue in degrees and
// saturation and lightness in [0, 1], each channel rounded half away from zero
function hslColorHex(hue: number, saturation: number, lightness: number): string {
  const c = (1 - Math.abs(2 * lightness - 1)) * saturation;
  const h = hue / 60;
  const x = c * (1 - Math.abs((h % 2) - 1));
  const m = lightness - c / 2;
  let r: number;
  let g: number;
  let b: number;
  if (h < 1) {
    [r, g, b] = [c, x, 0];
  } else if (h < 2) {
    [r, g, b] = [x, c, 0];
  } else if (h < 3) {
    [r, g, b] = [0, c, x];
  } else if (h < 4) {
    [r, g, b] = [0, x, c];
  } else if (h < 5) {
    [r, g, b] = [x, 0, c];
  } else {
    [r, g, b] = [c, 0, x];
  }
  // Go's math.Round rounds half away from zero, Math.round half up; the
  // channels are never negative, where the two agree
  const channel = (v: number) => hex2(Math.round((v + m) * 255));
  return channel(r) + channel(g) + channel(b);
}

/**
 * The ring color (six lower case hex digits, no "#") of a hue from
 * `extenderHue`: HSL at 70 percent saturation and 55 percent lightness.
 */
export function extenderHueColorHex(hue: number): string {
  if (!(Number.isInteger(hue) && 0 <= hue && hue < 360)) {
    throw new RangeError(`hue must be an integer from 0 to 359, got ${hue}`);
  }
  return hslColorHex(hue, 0.7, 0.55);
}

/**
 * The ring color of an extender address (six lower case hex digits, no "#"),
 * the same as sdk `GetExtenderColorHex`, so an extender is the same color
 * here as in the apps. The same address written two ways is one color.
 *
 * @example
 * extenderColorHex("::ffff:203.0.113.7") === extenderColorHex("203.0.113.7") // true
 */
export function extenderColorHex(ip: string): string {
  return extenderHueColorHex(extenderHue(ip));
}
