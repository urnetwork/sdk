/**
 * JWT utility functions for parsing and extracting claims from JWT tokens.
 */

/**
 * Parses and extracts the client_id from a JWT token without verification.
 *
 * This function decodes the JWT payload and extracts the client_id claim,
 * which should be a valid UUID string. It does NOT verify the JWT signature.
 *
 * @param byJwt - The JWT token string to parse
 * @returns The client_id extracted from the JWT claims as a string
 * @throws Error if the JWT is invalid, missing client_id, or client_id has invalid type
 *
 * @example
 * ```typescript
 * try {
 *   const clientId = parseByJwtClientId(jwtToken);
 *   console.log('Client ID:', clientId);
 * } catch (error) {
 *   console.error('Failed to parse JWT:', error.message);
 * }
 * ```
 */
export function parseByJwtClientId(byJwt: string): string {
  try {
    // Split the JWT into its three parts: header, payload, signature
    const parts = byJwt.split(".");

    if (parts.length !== 3) {
      throw new Error("Invalid JWT format: expected 3 parts separated by dots");
    }

    // Decode the payload (second part)
    const payload = parts[1];

    // Base64 URL decode - JWT uses URL-safe base64 encoding
    const base64 = payload.replace(/-/g, "+").replace(/_/g, "/");
    const jsonPayload = decodeURIComponent(
      atob(base64)
        .split("")
        .map((c) => "%" + ("00" + c.charCodeAt(0).toString(16)).slice(-2))
        .join(""),
    );

    const claims = JSON.parse(jsonPayload);

    // Check if client_id exists
    if (!claims.client_id) {
      throw new Error("byJwt does not contain claim client_id");
    }

    // Validate that client_id is a string
    if (typeof claims.client_id !== "string") {
      throw new Error(
        `byJwt have invalid type for client_id: ${typeof claims.client_id}`,
      );
    }

    // Validate that client_id is a valid UUID format
    if (!isValidUUID(claims.client_id)) {
      throw new Error(`client_id is not a valid UUID: ${claims.client_id}`);
    }

    return claims.client_id;
  } catch (error) {
    if (error instanceof Error) {
      throw error;
    }
    throw new Error(`Failed to parse JWT: ${String(error)}`);
  }
}

/**
 * Validates if a string is a valid UUID (version 4 format).
 *
 * @param str - The string to validate
 * @returns true if the string is a valid UUID, false otherwise
 */
function isValidUUID(str: string): boolean {
  const uuidRegex =
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  return uuidRegex.test(str);
}

/**
 * Interface representing the standard JWT claims structure.
 */
export interface JWTClaims {
  client_id?: string;
  [key: string]: unknown;
}

/**
 * Parses a JWT token and returns all claims without verification.
 *
 * @param jwt - The JWT token string to parse
 * @returns The decoded JWT claims object
 * @throws Error if the JWT format is invalid
 *
 * @example
 * ```typescript
 * const claims = parseJWTClaims(jwtToken);
 * console.log('All claims:', claims);
 * ```
 */
export function parseJWTClaims(jwt: string): JWTClaims {
  try {
    const parts = jwt.split(".");

    if (parts.length !== 3) {
      throw new Error("Invalid JWT format: expected 3 parts separated by dots");
    }

    const payload = parts[1];
    const base64 = payload.replace(/-/g, "+").replace(/_/g, "/");
    const jsonPayload = decodeURIComponent(
      atob(base64)
        .split("")
        .map((c) => "%" + ("00" + c.charCodeAt(0).toString(16)).slice(-2))
        .join(""),
    );

    return JSON.parse(jsonPayload) as JWTClaims;
  } catch (error) {
    if (error instanceof Error) {
      throw error;
    }
    throw new Error(`Failed to parse JWT: ${String(error)}`);
  }
}
