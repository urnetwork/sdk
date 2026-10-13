// Which extenders to measure (lag.dev/PLAN.md L3), as lag.dev's track picks
// them, minus the page's random tie-breaks: a run from the command line
// should be repeatable.
//
// - Near: the nearest extenders, distinct cities first, up to 3.
// - Far: the rest of the count from extenders at least 1,500 km from the
//   visitor and 500 km from every near pick, chosen greedily to maximize
//   first the number of countries other than the visitor's, then the sum of
//   great-circle distances among all picks.
// - Spares: the next 4 by the same rule, for dots that fail to open.
// Without the visitor's location every pick is by the far rule.

import type { LagExtender } from "../../src/lag/api";

export interface Server {
  extenderId: string;
  host: string;
  city: string;
  region?: string;
  countryCode: string;
  countryName: string;
  latitude: number;
  longitude: number;
  /** from the visitor, km; NaN when either location is unknown */
  distanceKm: number;
}

export interface Visitor {
  city?: string;
  region?: string;
  countryCode?: string;
  countryName?: string;
  latitude?: number;
  longitude?: number;
  asn?: { number: number; organization: string };
}

export interface Selection {
  /** nearest first */
  servers: Server[];
  spares: Server[];
}

export interface SelectOptions {
  /** how many to measure, 1 to 8 (default 8) */
  count?: number;
  /** spares for those that fail to open (default 4) */
  spares?: number;
}

const EARTH_RADIUS_KM = 6371.0088;
const NEAR = 3;
const FAR_FROM_VISITOR_KM = 1500;
const FAR_FROM_NEAR_KM = 500;

/** Great-circle distance in km. */
export function haversineKm(lat1: number, lon1: number, lat2: number, lon2: number): number {
  const rad = Math.PI / 180;
  const dLat = (lat2 - lat1) * rad;
  const dLon = (lon2 - lon1) * rad;
  const a = Math.sin(dLat / 2) ** 2 + Math.cos(lat1 * rad) * Math.cos(lat2 * rad) * Math.sin(dLon / 2) ** 2;
  return 2 * EARTH_RADIUS_KM * Math.asin(Math.min(1, Math.sqrt(a)));
}

const located = (latitude: number | undefined, longitude: number | undefined): boolean =>
  Number.isFinite(latitude) && Number.isFinite(longitude);

const between = (a: Server, b: Server): number =>
  located(a.latitude, a.longitude) && located(b.latitude, b.longitude)
    ? haversineKm(a.latitude, a.longitude, b.latitude, b.longitude)
    : 0;

/** The extenders A4 offered, one per id, as servers placed relative to the visitor. */
export function asServers(visitor: Visitor, extenders: readonly LagExtender[]): Server[] {
  const seen = new Set<string>();
  const servers: Server[] = [];
  for (const extender of extenders) {
    if (!extender.extender_id || !extender.host || seen.has(extender.extender_id)) {
      continue;
    }
    seen.add(extender.extender_id);
    const location = extender.location ?? {};
    const latitude = typeof location.latitude === "number" ? location.latitude : Number.NaN;
    const longitude = typeof location.longitude === "number" ? location.longitude : Number.NaN;
    servers.push({
      extenderId: extender.extender_id,
      host: extender.host,
      city: location.city ?? "",
      region: location.region,
      countryCode: (location.country_code ?? "").toLowerCase(),
      countryName: location.country_name ?? "",
      latitude,
      longitude,
      distanceKm:
        located(visitor.latitude, visitor.longitude) && located(latitude, longitude)
          ? haversineKm(visitor.latitude!, visitor.longitude!, latitude, longitude)
          : Number.NaN,
    });
  }
  return servers;
}

// Greedy far picks: countries other than the visitor's first, then distance.
function pickFar(fixed: Server[], pool: Server[], count: number, visitorCountry: string | undefined): Server[] {
  const picks: Server[] = [];
  const remaining = [...pool];
  while (picks.length < count && remaining.length > 0) {
    let bestIndex = -1;
    let bestCountry = -1;
    let bestDistance = -1;
    for (let i = 0; i < remaining.length; i += 1) {
      const candidate = remaining[i];
      const country = candidate.countryCode;
      const isNew =
        country !== "" &&
        country !== visitorCountry &&
        ![...fixed, ...picks].some((p) => p.countryCode === country)
          ? 1
          : 0;
      let distance = 0;
      for (const p of [...fixed, ...picks]) {
        distance += between(p, candidate);
      }
      if (isNew > bestCountry || (isNew === bestCountry && distance > bestDistance + 1e-9)) {
        bestIndex = i;
        bestCountry = isNew;
        bestDistance = distance;
      }
    }
    picks.push(remaining.splice(bestIndex, 1)[0]);
  }
  return picks;
}

/** Choose the servers to measure and their spares (L3, deterministic). */
export function selectServers(visitor: Visitor, extenders: readonly LagExtender[], options: SelectOptions = {}): Selection {
  const count = Math.max(1, Math.min(8, Math.floor(options.count ?? 8)));
  const spareCount = Math.max(0, Math.floor(options.spares ?? 4));
  const candidates = asServers(visitor, extenders).sort(
    (a, b) => a.extenderId.localeCompare(b.extenderId),
  );
  if (candidates.length === 0) {
    return { servers: [], spares: [] };
  }
  const visitorLocated = located(visitor.latitude, visitor.longitude);
  const visitorCountry = visitorLocated ? visitor.countryCode?.toLowerCase() : undefined;

  const near: Server[] = [];
  if (visitorLocated) {
    const nearest = candidates
      .filter((c) => Number.isFinite(c.distanceKm))
      .sort((a, b) => a.distanceKm - b.distanceKm || a.extenderId.localeCompare(b.extenderId));
    const nearCount = Math.min(NEAR, count);
    const cities = new Set<string>();
    const cityOf = (c: Server) => `${c.countryCode}/${c.city}`;
    for (const c of nearest.slice(0, 8)) {
      if (near.length < nearCount && !cities.has(cityOf(c))) {
        near.push(c);
        cities.add(cityOf(c));
      }
    }
    for (const c of nearest) {
      if (near.length < nearCount && !near.includes(c)) {
        near.push(c);
      }
    }
  }

  const rest = candidates.filter((c) => !near.includes(c));
  const eligible = (c: Server) =>
    !visitorLocated ||
    (c.distanceKm >= FAR_FROM_VISITOR_KM && near.every((n) => between(n, c) >= FAR_FROM_NEAR_KM));
  const farPool = rest.filter(eligible);
  const otherPool = rest.filter((c) => !eligible(c));
  let far = pickFar(near, farPool, count - near.length, visitorCountry);
  if (near.length + far.length < count) {
    far = [...far, ...pickFar([...near, ...far], otherPool, count - near.length - far.length, visitorCountry)];
  }

  const servers = [...near, ...far].sort((a, b) =>
    visitorLocated ? a.distanceKm - b.distanceKm : a.longitude - b.longitude || a.extenderId.localeCompare(b.extenderId),
  );
  const taken = new Set(servers.map((s) => s.extenderId));
  const leftover = [...farPool, ...otherPool].filter((c) => !taken.has(c.extenderId));
  const spares = pickFar(servers, leftover, spareCount, visitorCountry);
  return { servers, spares };
}
