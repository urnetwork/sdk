// A measurement as text for a person at a terminal. Machines get the same
// object as JSON (--json).

import type { Measurement, ServerResult } from "./measure";
import type { Rpm } from "./rpm";

const number = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });
const decimal = new Intl.NumberFormat("en-US", { minimumFractionDigits: 1, maximumFractionDigits: 1 });

const ms = (value: number | null | undefined): string => (value === null || value === undefined ? "-" : decimal.format(value));
const signedMs = (value: number | null | undefined): string =>
  value === null || value === undefined ? "-" : `${value < 0 ? "-" : "+"}${decimal.format(Math.abs(value))}`;
const mbps = (value: number | null | undefined): string => (value === null || value === undefined ? "-" : decimal.format(value));
const rpm = (value: Rpm | null | undefined): string => (value ? `${number.format(value.value)} ${value.band}` : "-");

/** Bytes as MB (10^6), the unit lag.dev states its data use in. */
export function megabytes(bytes: number): string {
  return `${number.format(Math.round(bytes / 1e6))} MB`;
}

const placeOf = (server: Pick<ServerResult, "city" | "countryCode" | "extenderId">): string =>
  [server.city, server.countryCode.toUpperCase()].filter((part) => part !== "").join(", ") || server.extenderId.slice(0, 8);

const table = (rows: string[][], right: ReadonlySet<number>): string[] => {
  const widths = rows[0].map((_, column) => Math.max(...rows.map((row) => row[column].length)));
  return rows.map((row) =>
    `  ${row.map((cell, column) => (right.has(column) ? cell.padStart(widths[column]) : cell.padEnd(widths[column]))).join("  ")}`.trimEnd(),
  );
};

const STOPPED: Record<string, string> = {
  max_bytes: "stopped early at the data cap",
  max_seconds: "stopped early at the time cap",
  interrupted: "stopped early: interrupted",
};

export function formatMeasurement(m: Measurement): string {
  const lines: string[] = [];
  const visitor = m.visitor;
  const from = visitor
    ? [
        [visitor.city, visitor.region, visitor.countryCode?.toUpperCase()].filter(Boolean).join(", "),
        visitor.asn ? `AS${visitor.asn.number} ${visitor.asn.organization}` : "",
      ]
        .filter(Boolean)
        .join(" · ")
    : "";
  const seconds = `${Math.round(m.durationMs / 1000)} s`;
  const measured = m.servers.filter((server) => !server.error).length;
  const head = [`lag.dev ${m.mode}`, from && `from ${from}`, `${measured} servers`, seconds];
  if (m.mode === "bufferbloat") {
    head.push(`${megabytes(m.bytes.total)}${m.bytes.maxBytes ? ` of ${megabytes(m.bytes.maxBytes)}` : ""}`);
  }
  lines.push(head.filter(Boolean).join(" · "), "");

  if (m.mode === "latency") {
    const rows = [["server", "km", "p50 ms", "p95 ms", "lost", "pings", "RPM"]];
    for (const s of m.servers) {
      rows.push(
        s.error
          ? [placeOf(s), "", "", "", "", "", `not measured: ${s.error}`]
          : [placeOf(s), s.distanceKm === null ? "" : number.format(s.distanceKm), ms(s.idle?.p50Ms), ms(s.idle?.p95Ms), String(s.lost), String(s.pings), rpm(s.rpm)],
      );
    }
    lines.push(...table(rows, new Set([1, 2, 3, 4, 5])));
  } else {
    const rows = [["server", "idle p50", "down Mbps", "down +ms", "up Mbps", "up +ms", "loaded RPM", ""]];
    for (const s of m.servers) {
      const load = s.load;
      rows.push(
        s.error
          ? [placeOf(s), "", "", "", "", "", "", `not measured: ${s.error}`]
          : [
              placeOf(s),
              ms(s.idle?.p50Ms),
              mbps(load?.download?.p50Mbps),
              signedMs(load?.download?.increaseMs),
              mbps(load?.upload?.p50Mbps),
              signedMs(load?.upload?.increaseMs),
              rpm(load?.rpm),
              load && load.status !== "complete" ? `${load.status}${load.reason ? `: ${load.reason}` : ""}` : "",
            ],
      );
    }
    lines.push(...table(rows, new Set([1, 2, 3, 4, 5])));
  }

  lines.push("");
  const o = m.overall;
  if (o.best) {
    lines.push(`  best: ${o.best.city || o.best.extenderId.slice(0, 8)} at ${ms(o.best.idleP50Ms)} ms · idle RPM ${rpm(o.rpm)}`);
  }
  if (o.idle) {
    lines.push(`  all servers: p50 ${ms(o.idle.p50Ms)} ms · p95 ${ms(o.idle.p95Ms)} ms · ${o.lost} of ${o.pings} pings lost`);
  }
  if (m.mode === "bufferbloat") {
    lines.push(
      `  loaded: download ${signedMs(o.loadedIncreaseMs?.download)} ms · upload ${signedMs(o.loadedIncreaseMs?.upload)} ms (median over servers) · loaded RPM ${rpm(o.loadedRpm)}`,
      `  speed: download ${mbps(o.downloadMbps?.best)} Mbps · upload ${mbps(o.uploadMbps?.best)} Mbps (best server's p50)`,
    );
  }
  if (m.stoppedEarly) {
    lines.push(`  ${STOPPED[m.stoppedEarly] ?? `stopped early: ${m.stoppedEarly}`}`);
  }
  return `${lines.join("\n")}\n`;
}
