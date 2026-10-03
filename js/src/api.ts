import type {
  AuthCodeLoginArgs,
  AuthCodeLoginResult,
  AuthLoginArgs,
  AuthLoginResult,
  AuthLoginWithPasswordArgs,
  AuthLoginWithPasswordResult,
  AuthNetworkClientArgs,
  AuthNetworkClientResult,
  AuthVerifyArgs,
  AuthVerifyResult,
  CreateApiKeyArgs,
  CreateApiKeyResult,
  DeleteApiKeyArgs,
  DeleteApiKeyResult,
  FindLocationsArgs,
  FindLocationsResult,
  ListApiKeysResult,
  NetworkCheckArgs,
  NetworkCheckResult,
  NetworkCreateArgs,
  NetworkCreateResult,
  RemoveNetworkClientArgs,
  RemoveNetworkClientResult,
} from "./generated";
import { URNetworkApiClient } from "./generated/client";
import type * as OpenAPI from "./generated/openapi";
import { isURNetworkApiError, type URNetworkApiClientConfig } from "./api_client";

export type URNetworkAPIConfig = URNetworkApiClientConfig;

// The legacy methods are typed with the Go-reflected types
// (src/generated/types.ts, from the sdk structs) and the generated client
// with the OpenAPI types (src/generated/openapi.ts). Both describe the same
// JSON, but not with the same strictness: the Go types mark every
// non-omitempty field required and every pointer `| null`, the spec marks
// few fields required, narrows some strings to enums and never says null.
// Neither is assignable to the other, so the boundary is this one explicit,
// runtime-free re-typing of the same JSON value.
const wire = <To>(value: unknown): To => value as To;

const errorMessage = (error: unknown, fallback: string): string =>
  error instanceof Error ? error.message : fallback;

// the legacy result-shaped error: an http failure reads as its status, any
// other failure (network, timeout, unparseable body) as its message
const legacyMessage = (error: unknown, fallback: string): string =>
  isURNetworkApiError(error) && error.kind === "http"
    ? `HTTP error! status: ${error.status}`
    : errorMessage(error, fallback);

const logFailure = (label: string, error: unknown) => {
  if (isURNetworkApiError(error) && error.kind === "http") {
    console.error(`${label} failed:`, error.status, error.statusText);
    console.error("Error response:", error.bodyText);
  } else {
    console.error(`${label} error:`, error);
  }
};

/**
 * The hand-picked api surface the React hooks use, on top of the generated
 * client. Each method keeps its original contract: most resolve with an
 * `{ error: { message } }` result instead of rejecting, and take the JWT
 * per call.
 *
 * For every other operation use `api.client` (the generated
 * URNetworkApiClient), which is configured with the same baseURL and, when
 * given, `config.token`.
 */
export class URNetworkAPI {
  /** the generated client for every operation in the OpenAPI spec */
  readonly client: URNetworkApiClient;

  constructor(config?: URNetworkAPIConfig) {
    this.client = new URNetworkApiClient(config);
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
      const result = await this.client.authLogin(
        wire<OpenAPI.AuthLoginArgs>({
          user_auth: params.user_auth,
          auth_jwt_type: params.auth_jwt_type,
          auth_jwt: params.auth_jwt,
          wallet_auth: params.wallet_auth,
        }),
      );
      return wire<AuthLoginResult>(result);
    } catch (error) {
      logFailure("Login", error);
      return {
        error: { message: legacyMessage(error, "Authentication failed") },
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
      const result = await this.client.authLoginWithPassword({
        user_auth: params.user_auth,
        password: params.password,
      });
      return wire<AuthLoginWithPasswordResult>(result);
    } catch (error) {
      logFailure("Password login", error);
      return {
        error: {
          message: legacyMessage(error, "Password authentication failed"),
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
      const result = await this.client.authNetworkCheck({
        network_name: params.network_name,
      });
      return wire<NetworkCheckResult>(result);
    } catch (error) {
      logFailure("Network check", error);
      return undefined;
    }
  }

  /**
   * Create network
   */
  async networkCreate(params: NetworkCreateArgs): Promise<NetworkCreateResult> {
    if (!params.terms) {
      return {
        error: {
          message: "Terms must be accepted to create a network.",
        },
      };
    }

    const requestParams: NetworkCreateArgs = {
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

    try {
      // POST /auth/network-create. The hand-written wrapper posted to
      // /network/create, a route the api never served (it 404ed).
      const result = await this.client.authNetworkCreate(
        wire<OpenAPI.NetworkCreateArgs>(requestParams),
      );
      return wire<NetworkCreateResult>(result);
    } catch (error) {
      logFailure("Network creation", error);
      return {
        error: { message: legacyMessage(error, "Network creation failed") },
      };
    }
  }

  async authCodeLogin(params: AuthCodeLoginArgs): Promise<AuthCodeLoginResult> {
    try {
      const result = await this.client.authCodeLogin(params);
      return wire<AuthCodeLoginResult>(result);
    } catch (error) {
      logFailure("Auth code login", error);
      return {
        by_jwt: "",
        error: {
          // this endpoint has always surfaced the raw error body
          message:
            isURNetworkApiError(error) && error.kind === "http"
              ? error.bodyText
              : errorMessage(error, "Auth code login failed"),
        },
      };
    }
  }

  /**
   * Fetches all network provider locations
   */
  async networkProviderLocations(): Promise<FindLocationsResult> {
    try {
      const result = await this.client.networkProviderLocations();
      return wire<FindLocationsResult>(result);
    } catch (error) {
      logFailure("/network/provider-locations", error);
      if (isURNetworkApiError(error) && error.kind === "http") {
        throw new Error(
          `Failed to fetch provider locations: ${error.status} ${error.statusText}`,
        );
      }
      // Re-throw the error so the caller can handle it
      throw error;
    }
  }

  async searchProviderLocations(
    params: FindLocationsArgs,
  ): Promise<FindLocationsResult> {
    try {
      const result = await this.client.networkFindProviderLocations(params);
      return wire<FindLocationsResult>(result);
    } catch (error) {
      logFailure("network/find-provider-locations", error);
      if (isURNetworkApiError(error) && error.kind === "http") {
        throw new Error(
          `Failed to search provider locations: ${error.status} ${error.statusText}`,
        );
      }
      // Re-throw the error so the caller can handle it
      throw error;
    }
  }

  /* ================
   *
   * authed endpoints
   *
   * ================ */

  async verifyUserAuth(
    params: AuthVerifyArgs,
    adminToken: string,
  ): Promise<AuthVerifyResult> {
    try {
      const result = await this.client.authVerify(
        {
          user_auth: params.user_auth,
          verify_code: params.verify_code,
        },
        { token: adminToken },
      );
      return wire<AuthVerifyResult>(result);
    } catch (error) {
      logFailure("User auth verification", error);
      return {
        error: { message: legacyMessage(error, "Verification failed") },
      };
    }
  }

  async authNetworkClient(
    params: AuthNetworkClientArgs,
    token: string,
    signal?: AbortSignal,
  ): Promise<AuthNetworkClientResult> {
    try {
      const result = await this.client.authNetworkClient(
        wire<OpenAPI.AuthNetworkClientArgs>(params),
        { token, signal },
      );
      return wire<AuthNetworkClientResult>(result);
    } catch (error) {
      // an abort rejects with the signal's reason untouched
      if (
        signal?.aborted ||
        (error instanceof Error && error.name === "AbortError")
      ) {
        console.log("Auth network client request was cancelled");
        throw error;
      }
      logFailure("Auth network client", error);
      return {
        proxy_config_result: null,
        error: {
          message: legacyMessage(error, "Auth network client failed"),
          client_limit_exceeded: false,
        },
      };
    }
  }

  async removeNetworkClient(
    params: RemoveNetworkClientArgs,
    token: string,
  ): Promise<RemoveNetworkClientResult> {
    try {
      const result = await this.client.removeNetworkClient(
        wire<OpenAPI.RemoveNetworkClientArgs>(params),
        { token },
      );
      return wire<RemoveNetworkClientResult>(result);
    } catch (error) {
      logFailure("Remove network client", error);
      return {
        error: {
          message: legacyMessage(error, "Remove network client failed"),
        },
      };
    }
  }

  async createApiKey(
    params: CreateApiKeyArgs,
    token: string,
  ): Promise<CreateApiKeyResult> {
    try {
      const result = await this.client.accountCreateApiKey(params, { token });
      return wire<CreateApiKeyResult>(result);
    } catch (error) {
      logFailure("Create API key", error);
      return {
        error: { message: legacyMessage(error, "Create API key failed") },
      };
    }
  }

  async listApiKeys(token: string): Promise<ListApiKeysResult> {
    try {
      const result = await this.client.accountGetApiKeys({ token });
      return wire<ListApiKeysResult>(result);
    } catch (error) {
      logFailure("List API keys", error);
      return {
        error: { message: legacyMessage(error, "List API keys failed") },
      };
    }
  }

  async deleteApiKey(
    params: DeleteApiKeyArgs,
    token: string,
  ): Promise<DeleteApiKeyResult> {
    try {
      const result = await this.client.accountRemoveApiKey(
        wire<OpenAPI.DeleteApiKeyArgs>(params),
        { token },
      );
      return wire<DeleteApiKeyResult>(result);
    } catch (error) {
      logFailure("Delete API key", error);
      return {
        error: { message: legacyMessage(error, "Delete API key failed") },
      };
    }
  }
}
