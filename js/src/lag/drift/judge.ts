// The verdict's geometry (lag.dev/DRIFTTEST.md V6), as connect's lagdrift
// computes it (judge.go): a visitor is outside their IP country only when the
// whole uncertainty region, the ellipse {q : (q − p̂)ᵀ Σ⁻¹ (q − p̂) ≤ k²}
// around the fit, lies outside it.
//
// Inside is tested in degrees (polygon.ts). Otherwise the region touches the
// country exactly when it contains a point of its boundary. Every ring of
// every polygon is projected onto the azimuthal equidistant plane centered on
// p̂, whitened with Σ = LLᵀ, and searched for the point q nearest p̂:
// D = ‖L⁻¹(q − p̂)‖. D ≤ k overlaps, D > k is outside. No convexity is
// assumed, so concave borders, holes and islands are all exact, and an island
// inside the region counts as touching.
//
// An edge is straight in degrees, as Natural Earth's are, so before
// projecting, an edge longer than 0.25° in latitude or longitude is split into
// equal parts no longer than that. A part whose ends are both more than
// 10,000 km from p̂ is taken at its ends alone: near the antipode the
// projection stretches a short edge into a long chord that can pass near the
// center.

import { Azimuthal, distanceKm as greatCircleKm, fromAzimuthalKm, hypot, type Point } from "./geo";
import { covEllipse, type Cov2, type Fit } from "./fit";
import { defaultConfidence } from "./model";
import { countryContains, countryIndex, type Country } from "./polygon";

// an edge is split into parts no longer than this, degrees
const maxEdgeDeg = 0.25;
// a part with both ends this far from the fit is taken at its ends, km
const farKm = 10000;
// a covariance's whitening floors its variances here, km²
const minVarianceKm2 = 1e-9;

/**
 * The k of a confidence: √χ²₂(confidence) = √(−2 ln(1 − confidence)). 2.448
 * at 95%, 3.035 at 99% and 3.717 at 99.9%. 0 for a confidence of 0 or less,
 * and Infinity for 1 or more.
 */
export function kForConfidence(confidence: number): number {
  if (!(0 < confidence)) {
    return 0;
  }
  if (1 <= confidence) {
    return Infinity;
  }
  return Math.sqrt(-2 * Math.log(1 - confidence));
}

/** The region test of a fit against a country (V6). */
export interface Judgement {
  /** the fit is in the country */
  inside: boolean;
  /** the region touches the country: inside, or D ≤ k */
  overlaps: boolean;
  /** the Mahalanobis distance to the nearest point of the country's boundary, inside or out */
  d: number;
  /** ‖q − p̂‖ to that point, km; 0 inside */
  distanceKm: number;
  /** k / √(uᵀΣ⁻¹u): the region's radius toward that point, km */
  significanceKm: number;
  /** distanceKm − significanceKm: positive exactly when the whole region is outside, km */
  marginKm: number;
  /** an extension of I3: the k the confidence gave */
  k: number;
}

/**
 * The region test of a fit against a country at a confidence (V6). The
 * figures are toward q, the boundary point nearest the fit in whitened
 * space, with u the unit vector from p̂ to q:
 *
 *   distanceKm = ‖q − p̂‖, 0 when inside
 *   significanceKm = k / √(uᵀΣ⁻¹u) = k ‖q − p̂‖ / D
 *   marginKm = distanceKm − significanceKm
 *
 * Outside, marginKm is computed as ‖q − p̂‖ (D − k) / D, so its sign is
 * exactly D > k's. Inside, it is −significanceKm. When p̂ is on the boundary
 * itself, u is undefined and significanceKm is the region's smallest radius,
 * k σ_minor. A null or empty country is never touched: d, distanceKm and
 * marginKm are Infinity.
 */
export function judge(fit: Fit | null, country: Country | null, confidence: number = defaultConfidence): Judgement {
  return judgeK(fit, country, kForConfidence(confidence));
}

export function judgeK(fit: Fit | null, country: Country | null, k: number): Judgement {
  const untouched = (): Judgement => ({
    inside: false,
    overlaps: false,
    d: Infinity,
    distanceKm: Infinity,
    significanceKm: 0,
    marginKm: Infinity,
    k,
  });
  if (fit === null || country === null) {
    return untouched();
  }
  const search = new BoundarySearch(fit.at, fit.cov);
  const sigmaMajorKm = search.sigmaMajorKm();
  const index = countryIndex(country);
  country.polygons.forEach((polygon, i) => {
    // a polygon whose every point is farther than the best in whitened space
    // can't hold the nearest
    if (index !== undefined && search.found) {
      const bound = index[i];
      if (search.best < (greatCircleKm(fit.at, bound.center) - bound.radiusKm) / sigmaMajorKm) {
        return;
      }
    }
    for (const ring of polygon) {
      search.ring(ring);
    }
  });
  if (!search.found) {
    return untouched();
  }
  const inside = countryContains(country, fit.at);
  const d = search.best;
  const [qEast, qNorth] = search.nearestKm();
  const distanceKm = hypot(qEast, qNorth);

  let significanceKm: number;
  if (0 < d && 0 < distanceKm) {
    significanceKm = (k * distanceKm) / d;
  } else {
    significanceKm = k * covEllipse(fit.cov).minorKm;
  }
  const j: Judgement = { inside, overlaps: inside || d <= k, d, distanceKm: 0, significanceKm, marginKm: 0, k };
  if (inside) {
    j.distanceKm = 0;
    j.marginKm = -significanceKm;
  } else if (0 < d && 0 < distanceKm) {
    j.distanceKm = distanceKm;
    j.marginKm = (distanceKm * (d - k)) / d;
  } else {
    j.distanceKm = distanceKm;
    j.marginKm = distanceKm - significanceKm;
  }
  return j;
}

/** The boundary point nearest a place in whitened space, over the rings given to it. */
export class BoundarySearch {
  private readonly projection: Azimuthal;
  // Σ = LLᵀ, L = [[l11, 0], [l21, l22]]
  private readonly l11: number;
  private readonly l21: number;
  private readonly l22: number;

  found = false;
  /** the whitened distance of the nearest so far */
  best = Infinity;
  private bestWx = 0;
  private bestWy = 0;

  constructor(at: Point, cov: Cov2) {
    const s00 = Math.max(minVarianceKm2, cov[0][0]);
    const s01 = (cov[0][1] + cov[1][0]) / 2;
    this.l11 = Math.sqrt(s00);
    this.l21 = s01 / this.l11;
    this.l22 = Math.sqrt(Math.max(minVarianceKm2, cov[1][1] - this.l21 * this.l21));
    this.projection = new Azimuthal(at);
  }

  /** the whitening's largest axis, √λ_max(LLᵀ), km: no point is nearer in whitened space than its distance over this */
  sigmaMajorKm(): number {
    const s00 = this.l11 * this.l11;
    const s01 = this.l11 * this.l21;
    const s11 = this.l21 * this.l21 + this.l22 * this.l22;
    return Math.sqrt((s00 + s11) / 2 + hypot((s00 - s11) / 2, s01));
  }

  /** L⁻¹ applied to east and north km */
  private whiten(eastKm: number, northKm: number): [number, number] {
    const x = eastKm / this.l11;
    return [x, (northKm - this.l21 * x) / this.l22];
  }

  /** the nearest point found, east and north km from the place */
  nearestKm(): [number, number] {
    return [this.l11 * this.bestWx, this.l21 * this.bestWx + this.l22 * this.bestWy];
  }

  /** searches a closed ring's edges, starting with the one from its last point to its first */
  ring(ring: readonly Point[]): void {
    const n = ring.length;
    if (n === 0) {
      return;
    }
    let prev = ring[n - 1];
    let [prevE, prevN, prevD] = this.projection.project(prev);
    if (n === 1) {
      this.segment(prevE, prevN, prevD, prevE, prevN, prevD);
      return;
    }
    for (const cur of ring) {
      splitEdge(prev, cur, (p) => {
        const [e, nn, d] = this.projection.project(p);
        this.segment(prevE, prevN, prevD, e, nn, d);
        prevE = e;
        prevN = nn;
        prevD = d;
      });
      prev = cur;
    }
  }

  private segment(aEast: number, aNorth: number, aDistance: number, bEast: number, bNorth: number, bDistance: number): void {
    const [ax, ay] = this.whiten(aEast, aNorth);
    const [bx, by] = this.whiten(bEast, bNorth);
    if (farKm < aDistance && farKm < bDistance) {
      this.consider(ax, ay);
      this.consider(bx, by);
      return;
    }
    const dx = bx - ax;
    const dy = by - ay;
    const length2 = dx * dx + dy * dy;
    let t = 0.0;
    if (0 < length2) {
      t = Math.max(0, Math.min(1, -(ax * dx + ay * dy) / length2));
    }
    this.consider(ax + t * dx, ay + t * dy);
  }

  private consider(x: number, y: number): void {
    this.found = true;
    const d = hypot(x, y);
    if (d < this.best) {
      this.best = d;
      this.bestWx = x;
      this.bestWy = y;
    }
  }
}

/**
 * Calls f with each point an edge from a to b is split at, then b. The edge
 * is split into ⌈max(|Δlat|, |Δlon|) / 0.25°⌉ equal parts, straight in
 * degrees.
 */
export function splitEdge(a: Point, b: Point, f: (p: Point) => void): void {
  const dLat = b.lat - a.lat;
  const dLon = b.lon - a.lon;
  const parts = Math.max(1, Math.ceil(Math.max(Math.abs(dLat), Math.abs(dLon)) / maxEdgeDeg));
  for (let s = 1; s < parts; s++) {
    f({ lat: a.lat + (dLat * s) / parts, lon: a.lon + (dLon * s) / parts });
  }
  f(b);
}

/**
 * V8: the radius of a circle with the area of the fit's region at
 * `confidence`, k · √(σ_major · σ_minor), km: the ± of the readout.
 */
export function regionRadiusKm(fit: Fit, confidence: number = defaultConfidence): number {
  return kForConfidence(confidence) * Math.sqrt(fit.majorKm * fit.minorKm);
}

/**
 * V8: `count` places around the edge of the fit's region at `confidence`,
 * the ellipse a radar draws (dashed, through its own scale).
 */
export function regionOutline(fit: Fit, confidence: number = defaultConfidence, count = 48): Point[] {
  const k = kForConfidence(confidence);
  const S = fit.cov;
  const l11 = Math.sqrt(Math.max(minVarianceKm2, S[0][0]));
  const l21 = (S[0][1] + S[1][0]) / 2 / l11;
  const l22 = Math.sqrt(Math.max(minVarianceKm2, S[1][1] - l21 * l21));
  const outline: Point[] = [];
  for (let i = 0; i < count; i++) {
    const angle = (2 * Math.PI * i) / count;
    const x = k * Math.cos(angle);
    const y = k * Math.sin(angle);
    outline.push(fromAzimuthalKm(fit.at, l11 * x, l21 * x + l22 * y));
  }
  return outline;
}
