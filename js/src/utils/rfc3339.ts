/**
 * Strict RFC 3339 times, as Go's `time.Parse(time.RFC3339, …)` reads them.
 *
 * The crypto twins (crypto_pay.ts, crypto_invoice.ts) compare server times the
 * way their Go halves do, and `Date.parse` is far looser than Go: it reads
 * "2026-10-20 00:00:00" as a local time and rolls "T24:00:00" into the next
 * day. Not exported from the package.
 */

const RFC3339 = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:[.,](\d+))?(Z|[+-]\d{2}:\d{2})$/;

/** Milliseconds since the epoch, or null for anything Go would refuse. */
export function parseRfc3339(value: string): number | null {
  const match = RFC3339.exec(value);
  if (!match) return null;
  const [year, month, day, hour, minute, second] = match.slice(1, 7).map(Number);
  if (month < 1 || 12 < month || day < 1 || 23 < hour || 59 < minute || 59 < second) return null;
  const date = new Date(0);
  date.setUTCFullYear(year, month - 1, day);
  // a day the month does not have rolls into the next month
  if (date.getUTCMonth() !== month - 1 || date.getUTCDate() !== day) return null;
  const millis = match[7] ? Math.floor(Number(`0.${match[7]}`) * 1000) : 0;
  date.setUTCHours(hour, minute, second, millis);
  let time = date.getTime();
  const zone = match[8];
  if (zone !== "Z") {
    const offsetHours = Number(zone.slice(1, 3));
    const offsetMinutes = Number(zone.slice(4, 6));
    if (23 < offsetHours || 59 < offsetMinutes) return null;
    const sign = zone[0] === "-" ? -1 : 1;
    time -= sign * (offsetHours * 60 + offsetMinutes) * 60000;
  }
  return time;
}
