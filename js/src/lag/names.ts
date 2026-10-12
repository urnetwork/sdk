// The canonical text of an address and the measurement names of extenders
// (lag.dev/PLAN.md N, S2).
//
// The canonical text is what Go's netip.Addr.Unmap().String() prints: lower
// case, the shortest ipv6 form (RFC 5952: the leftmost longest run of two or
// more zero groups compressed, never a single one), and a v4-mapped address as
// plain ipv4. The parser accepts exactly what netip.ParseAddr accepts, so an
// address the apps can read is read here the same way and nothing else is.
// Both are pinned by vectors generated from Go (gen_lag.go, test/lag).

/** The zone measurement names are under unless configured otherwise (N1, C1). */
export const DEFAULT_EXTENDER_MEASUREMENT_ZONE = "extender.bringyour.com";

// a parsed address: 4 or 16 bytes, and the ipv6 scope zone ("" for none)
interface Addr {
  bytes: Uint8Array;
  zone: string;
}

const DOT = 0x2e;
const COLON = 0x3a;
const PERCENT = 0x25;

// netip.ParseAddr: the first '.', ':' or '%' decides what the text is
function parseAddr(text: string): Addr | null {
  for (let i = 0; i < text.length; i += 1) {
    switch (text.charCodeAt(i)) {
      case DOT:
        return parseIPv4(text);
      case COLON:
        return parseIPv6(text);
      case PERCENT:
        // a zone with no address
        return null;
    }
  }
  return null;
}

// netip's parseIPv4Fields over text[start, end): four decimal fields of at
// most 255, with no leading zeros, written to out[at, at + 4)
function parseIPv4Fields(text: string, start: number, end: number, out: Uint8Array, at: number): boolean {
  let value = 0;
  let field = 0;
  let digits = 0;
  for (let i = start; i < end; i += 1) {
    const c = text.charCodeAt(i);
    if (0x30 <= c && c <= 0x39) {
      if (digits === 1 && value === 0) {
        // a leading zero
        return false;
      }
      value = value * 10 + (c - 0x30);
      digits += 1;
      if (255 < value) {
        return false;
      }
    } else if (c === DOT) {
      if (i === start || i === end - 1 || text.charCodeAt(i - 1) === DOT) {
        // an empty field
        return false;
      }
      if (field === 3) {
        // too long
        return false;
      }
      out[at + field] = value;
      field += 1;
      value = 0;
      digits = 0;
    } else {
      return false;
    }
  }
  if (field < 3) {
    // too short
    return false;
  }
  out[at + 3] = value;
  return true;
}

function parseIPv4(text: string): Addr | null {
  const bytes = new Uint8Array(4);
  return parseIPv4Fields(text, 0, text.length, bytes, 0) ? { bytes, zone: "" } : null;
}

// netip's parseIPv6, with a cursor in place of re-slicing
function parseIPv6(text: string): Addr | null {
  let address = text;
  let zone = "";
  const percent = text.indexOf("%");
  if (percent !== -1) {
    address = text.slice(0, percent);
    zone = text.slice(percent + 1);
    if (zone === "") {
      // an explicit zone must not be empty
      return null;
    }
  }
  const n = address.length;
  const bytes = new Uint8Array(16);
  // the byte position of the "::", or -1
  let ellipsis = -1;
  let p = 0;
  if (2 <= n && address.charCodeAt(0) === COLON && address.charCodeAt(1) === COLON) {
    ellipsis = 0;
    p = 2;
    if (p === n) {
      // only "::"
      return { bytes, zone };
    }
  }
  let i = 0;
  while (i < 16) {
    // one group of at most four hex digits
    let off = 0;
    let acc = 0;
    for (; p + off < n; off += 1) {
      const c = address.charCodeAt(p + off);
      let digit: number;
      if (0x30 <= c && c <= 0x39) {
        digit = c - 0x30;
      } else if (0x61 <= c && c <= 0x66) {
        digit = c - 0x61 + 10;
      } else if (0x41 <= c && c <= 0x46) {
        digit = c - 0x41 + 10;
      } else {
        break;
      }
      acc = acc * 16 + digit;
      if (3 < off || 0xffff < acc) {
        return null;
      }
    }
    if (off === 0) {
      // a field with no digits
      return null;
    }
    if (p + off < n && address.charCodeAt(p + off) === DOT) {
      // an embedded ipv4 address, only in the last 32 bits
      if (ellipsis < 0 && i !== 12) {
        return null;
      }
      if (16 < i + 4) {
        return null;
      }
      if (!parseIPv4Fields(address, p, n, bytes, i)) {
        return null;
      }
      p = n;
      i += 4;
      break;
    }
    bytes[i] = acc >> 8;
    bytes[i + 1] = acc & 0xff;
    i += 2;
    p += off;
    if (p === n) {
      break;
    }
    if (address.charCodeAt(p) !== COLON || p + 1 === n) {
      // not a colon, or a colon at the end
      return null;
    }
    p += 1;
    if (address.charCodeAt(p) === COLON) {
      if (0 <= ellipsis) {
        // a second "::"
        return null;
      }
      ellipsis = i;
      p += 1;
      if (p === n) {
        break;
      }
    }
  }
  if (p !== n) {
    // trailing text
    return null;
  }
  if (i < 16) {
    if (ellipsis < 0) {
      // too short
      return null;
    }
    const shift = 16 - i;
    for (let j = i - 1; ellipsis <= j; j -= 1) {
      bytes[j + shift] = bytes[j];
    }
    bytes.fill(0, ellipsis, ellipsis + shift);
  } else if (0 <= ellipsis) {
    // a "::" that stands for no zero group
    return null;
  }
  return { bytes, zone };
}

// netip.Addr.Unmap: a v4-mapped v6 address (::ffff:0:0/96) becomes ipv4, and
// loses its zone with the mapping
function unmap(addr: Addr): Addr {
  const bytes = addr.bytes;
  if (bytes.length === 16 && bytes[10] === 0xff && bytes[11] === 0xff && bytes.subarray(0, 10).every((b) => b === 0)) {
    return { bytes: bytes.slice(12), zone: "" };
  }
  return addr;
}

// netip.Addr.String for an ipv4 address or an ipv6 address that is not
// v4-mapped, which is every address unmap returns
function formatAddr(addr: Addr): string {
  const bytes = addr.bytes;
  if (bytes.length === 4) {
    return `${bytes[0]}.${bytes[1]}.${bytes[2]}.${bytes[3]}`;
  }
  const groups: number[] = [];
  for (let g = 0; g < 8; g += 1) {
    groups.push((bytes[2 * g] << 8) | bytes[2 * g + 1]);
  }
  // the leftmost longest run of at least two zero groups
  let zeroStart = -1;
  let zeroEnd = -1;
  for (let g = 0; g < 8; g += 1) {
    let j = g;
    while (j < 8 && groups[j] === 0) {
      j += 1;
    }
    if (2 <= j - g && zeroEnd - zeroStart < j - g) {
      zeroStart = g;
      zeroEnd = j;
    }
  }
  let text = "";
  for (let g = 0; g < 8; g += 1) {
    if (g === zeroStart) {
      text += "::";
      g = zeroEnd;
      if (8 <= g) {
        break;
      }
    } else if (0 < g) {
      text += ":";
    }
    text += groups[g].toString(16);
  }
  return addr.zone === "" ? text : `${text}%${addr.zone}`;
}

/**
 * The canonical text of an address, as Go's
 * `netip.ParseAddr(ip).Unmap().String()` gives it: lower case, the shortest
 * ipv6 form (RFC 5952), and a v4-mapped address as plain ipv4. An ipv6 scope
 * zone is kept, as netip keeps it. Text netip.ParseAddr refuses -- including
 * surrounding space, ipv4 fields with leading zeros and a "::" that elides no
 * zero group -- gives null.
 *
 * @example
 * canonicalIp("2001:DB8:0:0:0:0:0:1") // "2001:db8::1"
 * canonicalIp("::ffff:203.0.113.7")   // "203.0.113.7"
 */
export function canonicalIp(ip: string): string | null {
  const addr = parseAddr(ip);
  return addr === null ? null : formatAddr(unmap(addr));
}

// Go's unicode.IsSpace, the set strings.TrimSpace trims: it differs from
// String.prototype.trim, which keeps U+0085 and trims U+FEFF
const GO_SPACE = "[\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]";
const GO_TRIM_SPACE = new RegExp(`^${GO_SPACE}+|${GO_SPACE}+$`, "g");

/** Go's `strings.TrimSpace`, for parity with the Go functions this package mirrors. */
export function trimSpaceGo(text: string): string {
  return text.replace(GO_TRIM_SPACE, "");
}

// the zone as names are written (connect lag_names.go
// extenderMeasurementZone): without case, surrounding space or a trailing
// dot, and the default when empty
function measurementZone(zone: string): string {
  let normalized = trimSpaceGo(zone);
  if (normalized.endsWith(".")) {
    normalized = normalized.slice(0, -1);
  }
  normalized = normalized.toLowerCase();
  return normalized === "" ? DEFAULT_EXTENDER_MEASUREMENT_ZONE : normalized;
}

// connect ExtenderMeasurementLabel of a parsed address
function addrLabel(addr: Addr): string {
  let label = formatAddr(unmap({ bytes: addr.bytes, zone: "" })).replace(/[.:]/g, "-");
  if (label.startsWith("-")) {
    label = `0${label}`;
  }
  if (label.endsWith("-")) {
    label = `${label}0`;
  }
  return label;
}

/**
 * The measurement label of an address (N1): its canonical text with '.' and
 * ':' replaced by '-', and a '0' where the label would start or end with '-'.
 * The ipv6 scope zone is dropped, as it is never part of a name. Text that is
 * not an address has no label, "".
 *
 * @example
 * extenderMeasurementLabel("2001:db8::") // "2001-db8--0"
 */
export function extenderMeasurementLabel(ip: string): string {
  const addr = parseAddr(ip);
  return addr === null ? "" : addrLabel(addr);
}

/**
 * The host an extender serves the measurement API (M) on for one of its
 * addresses (N1): the address's label, '.', and the zone. The zone is
 * normalized as connect normalizes it (no case, surrounding space or trailing
 * dot) and defaults to `extender.bringyour.com`. Text that is not an address
 * has no host, "", as the zero netip.Addr has no name in Go.
 *
 * @example
 * extenderMeasurementHost("203.0.113.7") // "203-0-113-7.extender.bringyour.com"
 * extenderMeasurementHost("::1")         // "0--1.extender.bringyour.com"
 */
export function extenderMeasurementHost(ip: string, zone: string = DEFAULT_EXTENDER_MEASUREMENT_ZONE): string {
  const label = extenderMeasurementLabel(ip);
  return label === "" ? "" : `${label}.${measurementZone(zone)}`;
}

/**
 * The address a measurement host is for, as canonical text, or null: the
 * inverse of `extenderMeasurementHost`, as connect's
 * ParseExtenderMeasurementName (N2). Only the canonical spelling parses, the
 * one `extenderMeasurementHost` gives for the address it decodes to: upper
 * case, an uncompressed or otherwise compressed ipv6, leading zeros, a missing
 * or extra '0' pad, a v4-mapped or scoped spelling, a trailing dot, another
 * zone and an extra label are all refused. The zone is normalized as
 * `extenderMeasurementHost` normalizes it; the host is not.
 *
 * @example
 * parseExtenderMeasurementHost("2001-db8--1.extender.bringyour.com") // "2001:db8::1"
 * parseExtenderMeasurementHost("2001-db8-0-0-0-0-0-1.extender.bringyour.com") // null
 */
export function parseExtenderMeasurementHost(host: string, zone: string = DEFAULT_EXTENDER_MEASUREMENT_ZONE): string | null {
  const suffix = `.${measurementZone(zone)}`;
  if (!host.endsWith(suffix)) {
    return null;
  }
  const label = host.slice(0, host.length - suffix.length);
  // the '0' pad needs no undoing, since "0::1" and "2001:db8::0" are valid
  // ipv6 text; any spelling but the canonical one has another canonical label,
  // or is no address at all, and the comparison refuses it
  for (const text of [label.replace(/-/g, "."), label.replace(/-/g, ":")]) {
    const addr = parseAddr(text);
    if (addr !== null && addrLabel(addr) === label) {
      return formatAddr(unmap(addr));
    }
  }
  return null;
}
