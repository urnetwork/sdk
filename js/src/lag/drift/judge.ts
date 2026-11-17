// V6: outside only when the whole region is outside (lag.dev/DRIFTTEST.md V6).
//
// The region is the uncertainty ellipse at the chosen confidence,
// {q : (q − p̂)ᵀ Σ⁻¹ (q − p̂) ≤ k²}, with k = √χ²₂(confidence). The IP
// country's polygons are taken into the azimuthal equidistant projection
// centered on p̂ and whitened with Σ = LLᵀ; the boundary point q nearest p̂
// in whitened space has the Mahalanobis distance D = ‖L⁻¹(q − p̂)‖. The
// region touches the country exactly when p̂ is inside it or D ≤ k. Every
// boundary point of every polygon is checked, so concave borders, holes and
// islands need no convexity.

import { fromEastNorth, toEastNorth, type Point } from "./geo";
import type { Fit } from "./fit";
import { defaultConfidence } from "./model";
import { countryContains, nearestOnSegment, type Country } from "./polygon";

/** V6: k = √χ²₂(confidence) = √(−2 ln(1 − confidence)): 2.448 at 95%, 3.035 at 99%, 3.717 at 99.9%. */
export function kForConfidence(confidence: number): number {
  return Math.sqrt(-2 * Math.log(1 - confidence));
}

/** V6: how a fit's region lies against a country. */
export interface Judgement {
  /** the fit is inside the country */
  inside: boolean;
  /** the region touches the country: inside, or D ≤ k */
  overlaps: boolean;
  /** the Mahalanobis distance from the fit to the country's nearest boundary point, inside or out */
  d: number;
  /** from the fit to that point, km; 0 inside */
  distanceKm: number;
  /** the region's radius toward that point, km: k / √(uᵀΣ⁻¹u) */
  significanceKm: number;
  /** distanceKm − significanceKm: positive exactly when the whole region is outside */
  marginKm: number;
}

/**
 * V6: whether the fit's region at `confidence` touches the country, and the
 * figures toward the boundary point q nearest the fit in whitened space,
 * with u the unit vector from the fit to q.
 */
export function judge(fit: Fit, country: Country, confidence: number = defaultConfidence): Judgement {
  const k = kForConfidence(confidence);
  const S = fit.cov;
  const l11 = Math.sqrt(S[0][0]);
  const l21 = S[0][1] / l11;
  const l22 = Math.sqrt(Math.max(0, S[1][1] - l21 * l21));
  let bestD = Infinity;
  let bestX = 0;
  let bestY = 0;
  for (const polygon of country.polygons) {
    for (const ring of polygon) {
      if (ring.length === 0) {
        continue;
      }
      // whitened: w = L⁻¹ (east, north)
      const xs = new Array<number>(ring.length);
      const ys = new Array<number>(ring.length);
      for (let i = 0; i < ring.length; i++) {
        const { eastKm, northKm } = toEastNorth(fit.at, ring[i]);
        const x = eastKm / l11;
        xs[i] = x;
        ys[i] = (northKm - l21 * x) / l22;
      }
      for (let i = 0; i < ring.length; i++) {
        const j = i + 1 < ring.length ? i + 1 : 0;
        const q = nearestOnSegment(xs[i], ys[i], xs[j], ys[j]);
        const D = Math.hypot(q.x, q.y);
        if (D < bestD) {
          bestD = D;
          bestX = q.x;
          bestY = q.y;
        }
      }
    }
  }
  const inside = countryContains(country, fit.at);
  if (!Number.isFinite(bestD)) {
    // a country with no boundary
    return { inside, overlaps: inside, d: Infinity, distanceKm: inside ? 0 : Infinity, significanceKm: 0, marginKm: inside ? 0 : Infinity };
  }
  // q = L w
  const qe = l11 * bestX;
  const qn = l21 * bestX + l22 * bestY;
  const distance = Math.hypot(qe, qn);
  let significance = 0;
  if (distance > 0) {
    const ue = qe / distance;
    const un = qn / distance;
    const det = S[0][0] * S[1][1] - S[0][1] * S[1][0];
    // uᵀ Σ⁻¹ u
    const quad = (ue * ue * S[1][1] - 2 * ue * un * S[0][1] + un * un * S[0][0]) / det;
    significance = k / Math.sqrt(quad);
  }
  const distanceKm = inside ? 0 : distance;
  return {
    inside,
    overlaps: inside || bestD <= k,
    d: bestD,
    distanceKm,
    significanceKm: significance,
    marginKm: distanceKm - significance,
  };
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
  const l11 = Math.sqrt(S[0][0]);
  const l21 = S[0][1] / l11;
  const l22 = Math.sqrt(Math.max(0, S[1][1] - l21 * l21));
  const outline: Point[] = [];
  for (let i = 0; i < count; i++) {
    const angle = (2 * Math.PI * i) / count;
    const x = k * Math.cos(angle);
    const y = k * Math.sin(angle);
    outline.push(fromEastNorth(fit.at, l11 * x, l21 * x + l22 * y));
  }
  return outline;
}
