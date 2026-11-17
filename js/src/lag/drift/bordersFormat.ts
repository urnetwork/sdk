// The borders file (lag.dev/DRIFTTEST.md G1, I5), as connect's lagdrift reads
// it (borders_format.go): Natural Earth's Admin 0 at 1:10m, keyed by
// GeoLite2's country codes, for the api's verdicts. The browser never needs
// it; it is here so the TypeScript model reads every file the Go one does.
// Integers are Go's varints: uvarint, and zigzag varint for signed ones.
//
//   magic          16 bytes, "lagdrift-borders"
//   format         uvarint, 1
//   version        uvarint length, then that many bytes of UTF-8
//   countries      uvarint count, then each country, sorted by code:
//     code         2 bytes, two lower-case ASCII letters
//     box          4 varints: the lowest latitude, lowest longitude, highest
//                  latitude and highest longitude of its points, microdegrees
//     polygons     uvarint count (at least 1), then each polygon:
//       rings      uvarint count (at least 1), then each ring:
//         points   uvarint count (at least 3), then each point as two
//                  varints, its latitude and longitude in microdegrees, less
//                  the previous point's in the ring
//   checksum       4 bytes, little-endian: the CRC-32 (IEEE) of every byte before it

import { Borders } from "./borders";
import type { Point } from "./geo";
import type { Country } from "./polygon";

/** The file's first bytes. */
export const bordersMagic = "lagdrift-borders";
/** The layout `readBorders` reads. */
export const bordersFormatVersion = 1;

const microdegreesPerDegree = 1000000;
const maxBordersVersionLen = 1024;
const minRingPoints = 3;

const invalid = (why: string) => new TypeError(`drift borders file: ${why}`);

let crcTable: Uint32Array | undefined;

/** CRC-32 (IEEE), as Go's crc32.ChecksumIEEE. */
export function crc32(bytes: Uint8Array): number {
  if (crcTable === undefined) {
    crcTable = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
      let c = n;
      for (let k = 0; k < 8; k++) {
        c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      }
      crcTable[n] = c >>> 0;
    }
  }
  let crc = 0xffffffff;
  for (const byte of bytes) {
    crc = crcTable[(crc ^ byte) & 0xff] ^ (crc >>> 8);
  }
  return (crc ^ 0xffffffff) >>> 0;
}

class Reader {
  private readonly data: Uint8Array;
  offset: number;

  constructor(data: Uint8Array, offset: number) {
    this.data = data;
    this.offset = offset;
  }

  remaining(): number {
    return this.data.length - this.offset;
  }

  uvarint(): number {
    let value = 0;
    let multiplier = 1;
    for (let i = 0; i < 10; i++) {
      if (this.data.length <= this.offset + i) {
        break;
      }
      const byte = this.data[this.offset + i];
      value += (byte & 0x7f) * multiplier;
      if (byte < 0x80) {
        if (!Number.isSafeInteger(value)) {
          break;
        }
        this.offset += i + 1;
        return value;
      }
      multiplier *= 128;
    }
    throw invalid(`a number at byte ${this.offset}`);
  }

  varint(): number {
    const ux = this.uvarint();
    return ux % 2 === 0 ? ux / 2 : -(ux + 1) / 2;
  }

  /** a count no larger than limit, so a damaged count can't allocate the earth */
  count(limit: number): number {
    const v = this.uvarint();
    if (Math.max(limit, 0) < v) {
      throw invalid(`a count of ${v} at byte ${this.offset}`);
    }
    return v;
  }

  bytes(n: number): Uint8Array {
    if (this.remaining() < n) {
      throw invalid("the file ends early");
    }
    const b = this.data.subarray(this.offset, this.offset + n);
    this.offset += n;
    return b;
  }
}

/**
 * Reads a borders file. Every count, code, coordinate and box is checked,
 * and the checksum, so a truncated or damaged file is an error (a TypeError)
 * rather than wrong borders.
 */
export function readBorders(data: Uint8Array): Borders {
  const magic = new TextEncoder().encode(bordersMagic);
  if (data.length < magic.length + 4 || !magic.every((byte, i) => data[i] === byte)) {
    throw invalid("not a borders file");
  }
  const body = data.subarray(0, data.length - 4);
  const stored = new DataView(data.buffer, data.byteOffset + data.length - 4, 4).getUint32(0, true);
  if (stored !== crc32(body)) {
    throw invalid("the checksum does not match");
  }
  const reader = new Reader(body, magic.length);
  const formatVersion = reader.uvarint();
  if (formatVersion !== bordersFormatVersion) {
    throw invalid(`format version ${formatVersion}, not ${bordersFormatVersion}`);
  }
  const versionLen = reader.count(maxBordersVersionLen);
  const version = new TextDecoder().decode(reader.bytes(versionLen));
  const countryCount = reader.count(reader.remaining());
  const countries: Country[] = [];
  for (let i = 0; i < countryCount; i++) {
    const code = String.fromCharCode(...reader.bytes(2));
    if (!/^[a-z]{2}$/.test(code)) {
      throw invalid(`country code ${JSON.stringify(code)}`);
    }
    if (0 < countries.length && code <= countries[countries.length - 1].code) {
      throw invalid(`country ${code} is out of order`);
    }
    const box = [reader.varint(), reader.varint(), reader.varint(), reader.varint()];
    const polygonCount = reader.count(reader.remaining());
    if (polygonCount === 0) {
      throw invalid(`country ${code} has no polygons`);
    }
    const seen = [Infinity, Infinity, -Infinity, -Infinity];
    const country: Country = { code, polygons: [] };
    for (let j = 0; j < polygonCount; j++) {
      const ringCount = reader.count(reader.remaining());
      if (ringCount === 0) {
        throw invalid(`a polygon of ${code} has no rings`);
      }
      const polygon: Point[][] = [];
      for (let k = 0; k < ringCount; k++) {
        // each point takes at least two bytes
        const pointCount = reader.count(Math.floor(reader.remaining() / 2));
        if (pointCount < minRingPoints) {
          throw invalid(`a ring of ${code} has ${pointCount} points`);
        }
        const ring: Point[] = [];
        let lat = 0;
        let lon = 0;
        for (let m = 0; m < pointCount; m++) {
          lat += reader.varint();
          lon += reader.varint();
          if (lat < -90 * microdegreesPerDegree || 90 * microdegreesPerDegree < lat || lon < -180 * microdegreesPerDegree || 180 * microdegreesPerDegree < lon) {
            throw invalid(`a point of ${code} is out of range`);
          }
          seen[0] = Math.min(seen[0], lat);
          seen[1] = Math.min(seen[1], lon);
          seen[2] = Math.max(seen[2], lat);
          seen[3] = Math.max(seen[3], lon);
          ring.push({ lat: lat / microdegreesPerDegree, lon: lon / microdegreesPerDegree });
        }
        polygon.push(ring);
      }
      country.polygons.push(polygon);
    }
    if (seen.some((v, b) => v !== box[b])) {
      throw invalid(`the box of ${code} does not match its points`);
    }
    countries.push(country);
  }
  if (reader.offset !== body.length) {
    throw invalid(`${body.length - reader.offset} bytes follow the last country`);
  }
  return new Borders(version, countries);
}
