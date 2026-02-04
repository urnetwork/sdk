import {
  AuthLoginArgs,
  AuthLoginWithPasswordArgs,
  AuthLoginWithPasswordResult,
  AuthVerifyArgs,
  AuthVerifyResult,
  NetworkCheckArgs,
  NetworkCheckResult,
  NetworkCreateArgs,
  NetworkCreateResult,
} from "./generated";
// we handle types with arrays manually due to gomobile limitations
import { AuthLoginResult } from "./types";

export class URNetworkAPI {
  private baseURL: string;
  // @ts-ignore - will add in with auth methods
  private token?: string;

  constructor(config?: { baseURL?: string; token?: string }) {
    this.baseURL = config?.baseURL || "https://api.bringyour.com";
    this.token = config?.token;
  }

  setToken(token: string) {
    this.token = token;
  }

  clearToken() {
    this.token = undefined;
  }

  /**
   * For protected endpoints, ensure that a token is set
   * @returns The current authentication token
   * @throws Error if no token is set
   */
  // @ts-ignore - will add in with auth methods
  private requireToken(): string {
    if (!this.token) {
      throw new Error("Authentication required. Please login first.");
    }
    return this.token;
  }

  /**
   * Safely parse JSON response with fallback to text on error
   * Prevents application crashes from malformed JSON
   */
  private async safeJsonParse<T>(response: Response): Promise<T> {
    const contentType = response.headers.get("content-type");

    // Handle empty responses (204 No Content)
    if (response.status === 204) {
      return {} as T;
    }

    // Only attempt JSON parsing if content-type is JSON
    if (contentType && contentType.includes("application/json")) {
      try {
        const text = await response.text();
        if (!text || text.trim() === "") {
          return {} as T;
        }
        return JSON.parse(text) as T;
      } catch (error) {
        console.error("JSON parse error:", error);
        throw new Error("Failed to parse response as JSON");
      }
    }

    // Non-JSON response
    const text = await response.text();
    throw new Error(
      `Expected JSON response but got: ${text.substring(0, 100)}`,
    );
  }

  /* ================
   *
   * public endpoints
   *
   * ================ */

  /**
   * Used for SSO or to check if a user_auth exists
   */
  async authLogin(params: AuthLoginArgs): Promise<AuthLoginResult> {
    try {
      const response = await fetch(`${this.baseURL}/auth/login`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          user_auth: params.user_auth,
          auth_jwt_type: params.auth_jwt_type,
          auth_jwt: params.auth_jwt,
          wallet_auth: params.wallet_auth,
        }),
      });

      if (!response.ok) {
        console.error(
          "Password login failed:",
          response.status,
          response.statusText,
        );
        const errorData = await response.text();
        console.error("Error response:", errorData);

        return {
          error: {
            message: `HTTP error! status: ${response.status}`,
          },
        };
      }

      const data = await this.safeJsonParse<AuthLoginResult>(response);

      /**
       * if successful login, set the token for future requests
       */
      if (data.network?.by_jwt) {
        this.setToken(data.network.by_jwt);
      }

      return data;
    } catch (error) {
      console.error("Login error:", error);
      return {
        error: {
          message:
            error instanceof Error ? error.message : "Authentication failed",
        },
      };
    }
  }

  /**
   * Login with Password
   */
  async authLoginWithPassword(
    params: AuthLoginWithPasswordArgs,
  ): Promise<AuthLoginWithPasswordResult> {
    try {
      const response = await fetch(`${this.baseURL}/auth/login-with-password`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          user_auth: params.user_auth,
          password: params.password,
        }),
      });

      if (!response.ok) {
        console.error(
          "Password login failed:",
          response.status,
          response.statusText,
        );
        const errorData = await response.text();
        console.error("Error response:", errorData);

        return {
          error: {
            message: `HTTP error! status: ${response.status}`,
          },
        };
      }

      const data =
        await this.safeJsonParse<AuthLoginWithPasswordResult>(response);

      /**
       * if successful login, set the token for future requests
       */
      if (data.network?.by_jwt) {
        this.setToken(data.network.by_jwt);
      }

      return data;
    } catch (error) {
      console.error("Password login error:", error);
      return {
        error: {
          message:
            error instanceof Error
              ? error.message
              : "Password authentication failed",
        },
      };
    }
  }

  /**
   * Check network name availability
   * note - this is an older endpoint, which doesn't have an "error" property
   * if there's an error, return undefined, and prompt an error in the UI
   */
  async networkCheck(
    params: NetworkCheckArgs,
  ): Promise<NetworkCheckResult | undefined> {
    try {
      const response = await fetch(`${this.baseURL}/auth/network-check`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          network_name: params.network_name,
        }),
      });

      if (!response.ok) {
        console.error(
          "Network check failed:",
          response.status,
          response.statusText,
        );
        const errorData = await response.text();
        console.error("Error response:", errorData);

        return undefined;
      }

      return await this.safeJsonParse<NetworkCheckResult>(response);
    } catch (error) {
      console.error("Network check error:", error);
      return undefined;
    }
  }

  /**
   * Create network
   */
  async networkCreate(params: NetworkCreateArgs): Promise<NetworkCreateResult> {
    try {
      if (!params.terms) {
        return {
          error: {
            message: "Terms must be accepted to create a network.",
          },
        };
      }

      let requestParams: NetworkCreateArgs = {
        terms: params.terms,
        guest_mode: false, // not allowing guest mode on web
      };

      // creating a network with user_auth + password
      if (params.user_auth && params.password) {
        requestParams.user_auth = params.user_auth;
        requestParams.password = params.password;
      }

      // creating a network with SSO
      if (params.auth_jwt && params.auth_jwt_type) {
        requestParams.auth_jwt = params.auth_jwt;
        requestParams.auth_jwt_type = params.auth_jwt_type;
      }

      // creating a network with solana wallet_auth
      if (params.wallet_auth) {
        requestParams.wallet_auth = params.wallet_auth;
      }

      const response = await fetch(`${this.baseURL}/network/create`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
        },
        body: JSON.stringify(requestParams),
      });

      if (!response.ok) {
        console.error(
          "Network creation failed:",
          response.status,
          response.statusText,
        );
        const errorData = await response.text();
        console.error("Error response:", errorData);

        return {
          error: {
            message: `HTTP error! status: ${response.status}`,
          },
        };
      }

      const data = await this.safeJsonParse<NetworkCreateResult>(response);

      /**
       * if successful creation, set the token for future requests
       */
      if (data.network?.by_jwt) {
        this.setToken(data.network.by_jwt);
      }

      return data;
    } catch (error) {
      console.error("Network creation error:", error);
      return {
        error: {
          message:
            error instanceof Error ? error.message : "Network creation failed",
        },
      };
    }
  }

  /* ================
   *
   * authed endpoints
   *
   * ================ */

  async verifyUserAuth(params: AuthVerifyArgs): Promise<AuthVerifyResult> {
    try {
      // requires the admin JWT
      const token = this.requireToken();

      const response = await fetch(`${this.baseURL}/auth/verify`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({
          user_auth: params.user_auth,
          verify_code: params.verify_code,
        }),
      });

      if (!response.ok) {
        console.error(
          "User auth verification failed:",
          response.status,
          response.statusText,
        );
        const errorData = await response.text();
        console.error("Error response:", errorData);

        return {
          error: {
            message: `HTTP error! status: ${response.status}`,
          },
        };
      }

      const data = await this.safeJsonParse<AuthVerifyResult>(response);

      if (data.network?.by_jwt) {
        /**
         * set the client JWT
         */
        this.setToken(data.network.by_jwt);
      }

      return data;
    } catch (error) {
      console.error("User auth verification error:", error);
      return {
        error: {
          message:
            error instanceof Error ? error.message : "Verification failed",
        },
      };
    }
  }
}
