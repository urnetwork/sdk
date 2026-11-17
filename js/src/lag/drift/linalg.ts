// Small dense linear algebra for the drift fit (at most 2 + 3 parameters, and
// blocks of at most 3 runs).

/**
 * Solves A x = v by Gaussian elimination with partial pivoting. `null` when
 * A is singular. Neither argument is changed.
 */
export function solve(A: readonly (readonly number[])[], v: readonly number[]): number[] | null {
  const n = v.length;
  const M = A.map((row, i) => [...row.slice(0, n), v[i]]);
  for (let c = 0; c < n; c++) {
    let p = c;
    for (let r = c + 1; r < n; r++) {
      if (Math.abs(M[r][c]) > Math.abs(M[p][c])) {
        p = r;
      }
    }
    if (!(Math.abs(M[p][c]) > 0) || !Number.isFinite(M[p][c])) {
      return null;
    }
    if (p !== c) {
      const t = M[c];
      M[c] = M[p];
      M[p] = t;
    }
    for (let r = c + 1; r < n; r++) {
      const f = M[r][c] / M[c][c];
      if (f !== 0) {
        for (let k = c; k <= n; k++) {
          M[r][k] -= f * M[c][k];
        }
      }
    }
  }
  const x = new Array<number>(n).fill(0);
  for (let r = n - 1; r >= 0; r--) {
    let s = M[r][n];
    for (let k = r + 1; k < n; k++) {
      s -= M[r][k] * x[k];
    }
    x[r] = s / M[r][r];
  }
  return x.every(Number.isFinite) ? x : null;
}

/** The inverse of A, `null` when A is singular. */
export function invert(A: readonly (readonly number[])[]): number[][] | null {
  const n = A.length;
  const columns: number[][] = [];
  for (let j = 0; j < n; j++) {
    const e = new Array<number>(n).fill(0);
    e[j] = 1;
    const column = solve(A, e);
    if (column === null) {
      return null;
    }
    columns.push(column);
  }
  return A.map((_, i) => columns.map((column) => column[i]));
}

/**
 * The inverse of D + τ² 11ᵀ, for a diagonal D of positive variances: an
 * extender's block of the pooled fit's covariance (V7), by Sherman–Morrison.
 */
export function invertSharedVariance(variances: readonly number[], shared: number): number[][] {
  let s = 0;
  for (const v of variances) {
    s += 1 / v;
  }
  const f = shared / (1 + shared * s);
  return variances.map((vj, j) => variances.map((vk, k) => (j === k ? 1 / vj : 0) - f / (vj * vk)));
}
