import React, {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { URNetworkAPI } from "../api";
import {
  AuthLoginArgs,
  AuthLoginResult,
  AuthLoginWithPasswordArgs,
  AuthLoginWithPasswordResult,
  NetworkCheckResult,
  NetworkCreateArgs,
  NetworkCreateResult,
} from "../generated";

interface URNetworkAPIContextValue {
  api: URNetworkAPI;
}

const URNetworkAPIContext = createContext<URNetworkAPIContextValue | null>(
  null,
);

export interface URNetworkAPIProviderProps {
  children: ReactNode;
  config?: { baseURL?: string; token?: string };
}

/**
 * Provider for URNetworkAPI - ensures single shared instance across app
 *
 * @example
 * ```tsx
 * import { URNetworkAPIProvider } from '@urnetwork/sdk-js/react';
 *
 * function App() {
 *   return (
 *     <URNetworkAPIProvider config={{ baseURL: 'https://api.bringyour.com' }}>
 *       <YourApp />
 *     </URNetworkAPIProvider>
 *   );
 * }
 * ```
 */
export function URNetworkAPIProvider({
  children,
  config,
}: URNetworkAPIProviderProps) {
  const apiRef = useRef<URNetworkAPI>();

  if (!apiRef.current) {
    apiRef.current = new URNetworkAPI(config);
  }

  return (
    // @ts-ignore
    <URNetworkAPIContext.Provider value={{ api: apiRef.current }}>
      {children}
    </URNetworkAPIContext.Provider>
  );
}

/**
 * Hook to get the shared URNetworkAPI instance from context
 * @throws Error if used outside URNetworkAPIProvider
 */
export function useAPI(): URNetworkAPI {
  const context = useContext(URNetworkAPIContext);
  if (!context) {
    throw new Error("useAPI must be used within URNetworkAPIProvider");
  }
  return context.api;
}

/**
 * Auth flow
 */
export interface NetworkInfo {
  byJwt: string;
  networkName?: string;
}

export interface AuthLoadingState {
  checkingNetwork: boolean;
  checkingUserAuth: boolean;
  loggingInWithGoogle: boolean;
  loggingInWithPassword: boolean;
  creatingNetwork: boolean;
  checkingNetworkName: boolean;
}

export interface AuthErrorState {
  checkNetwork: string | null;
  checkUserAuth: string | null;
  googleLogin: string | null;
  passwordLogin: string | null;
  createNetwork: string | null;
  networkCheck: string | null;
}

const initialLoadingState: AuthLoadingState = {
  checkingNetwork: false,
  checkingUserAuth: false,
  loggingInWithGoogle: false,
  loggingInWithPassword: false,
  creatingNetwork: false,
  checkingNetworkName: false,
};

const initialErrorState: AuthErrorState = {
  checkNetwork: null,
  checkUserAuth: null,
  googleLogin: null,
  passwordLogin: null,
  createNetwork: null,
  networkCheck: null,
};

/**
 * Authentication flow state
 */
export interface AuthFlowState {
  // step: AuthStep;
  // authMethod: AuthMethod;
  userAuth?: string; // Email or other user identifier
  authJwt?: string; // JWT from SSO provider
  authJwtType?: string; // Type of SSO (e.g., "google")
  networkInfo?: NetworkInfo;
  loading: AuthLoadingState;
  errors: AuthErrorState;
}

/**
 * Authentication flow context value
 */
interface AuthFlowContextValue {
  state: AuthFlowState;
  setUserAuth: (userAuth: string) => void;

  // Actions
  checkUserAuth: (userAuth: string) => Promise<AuthLoginResult>;
  loginWithPassword: (params: {
    password: string;
  }) => Promise<AuthLoginWithPasswordResult>;
  createNetwork: (params: {
    password?: string;
    networkName: string;
    terms: boolean;
  }) => Promise<NetworkCreateResult>;

  // checkNetwork: (networkName: string) => Promise<void>;
  // loginWithGoogle: (authJwt: string) => Promise<void>;
  // loginWithPassword: (password: string) => Promise<void>;
  // createNetwork: (params: {
  //   networkName?: string;
  //   password?: string;
  //   terms: boolean;
  // }) => Promise<void>;
  // reset: () => void;
  // clearError: () => void;
}

const AuthFlowContext = createContext<AuthFlowContextValue | null>(null);

const initialState: AuthFlowState = {
  userAuth: "",
  loading: initialLoadingState,
  errors: initialErrorState,
};

export interface NetworkCheckState {
  loading: boolean;
  error: string | null;
  result: NetworkCheckResult | null;
}

// const initialNetworkCheckState: NetworkCheckState = {
//   loading: false,
//   error: null,
//   result: null,
// };

export interface AuthFlowProviderProps {
  children: ReactNode;
}

export function AuthFlowProvider({ children }: AuthFlowProviderProps) {
  const api = useAPI();
  const [state, setState] = useState<AuthFlowState>(initialState);
  // const [userAuth, setUserAuthState] = useState("");
  // const [loading, setLoading] = useState<AuthLoadingState>(initialLoadingState);
  // const [errors, setErrors] = useState<AuthErrorState>(initialErrorState);
  // const [networkCheckState, setNetworkCheckState] = useState<NetworkCheckState>(
  //   initialNetworkCheckState,
  // );

  const checkUserAuth = useCallback(
    async (userAuth: string) => {
      setState((prev) => ({
        ...prev,
        loading: { ...prev.loading, checkingUserAuth: true },
        errors: { ...prev.errors, checkUserAuth: null },
      }));

      try {
        const params: AuthLoginArgs = {
          user_auth: userAuth,
        };

        const result = await api.authLogin(params);
        if (result.error) {
          const err = new Error(result.error.message);
          setState((prev) => ({
            ...prev,
            errors: { ...prev.errors, checkUserAuth: err.message },
          }));
          return result;
        }

        // success, track userAuth
        setState((prev) => ({
          ...prev,
          userAuth,
        }));

        return result;
      } catch (err) {
        setState((prev) => ({
          ...prev,
          errors: { ...prev.errors, checkUserAuth: (err as Error).message },
        }));
        throw err;
      } finally {
        setState((prev) => ({
          ...prev,
          loading: { ...prev.loading, checkingUserAuth: false },
        }));
      }
    },
    [api],
  );

  const loginWithPassword = useCallback(
    async (params: { password: string }) => {
      setState((prev) => ({
        ...prev,
        loading: { ...prev.loading, loggingInWithPassword: true },
        errors: { ...prev.errors, passwordLogin: null },
      }));

      try {
        const requestParams: AuthLoginWithPasswordArgs = {
          user_auth: state.userAuth || "",
          password: params.password,
        };

        const result = await api.authLoginWithPassword(requestParams);
        if (result.error) {
          const err = new Error(result.error.message);

          setState((prev) => ({
            ...prev,
            errors: { ...prev.errors, passwordLogin: err.message },
          }));

          return result;
        }
        return result;
      } catch (err) {
        setState((prev) => ({
          ...prev,
          errors: { ...prev.errors, passwordLogin: (err as Error).message },
        }));

        throw err;
      } finally {
        setState((prev) => ({
          ...prev,
          loading: { ...prev.loading, loggingInWithPassword: false },
        }));
      }
    },
    [api],
  );

  const setUserAuth = useCallback((userAuth: string) => {
    setState((prev) => ({ ...prev, userAuth }));
  }, []);

  const createNetwork = useCallback(
    async (params: {
      password?: string;
      networkName: string;
      terms: boolean;
    }) => {
      setState((prev) => ({
        ...prev,
        loading: { ...prev.loading, creatingNetwork: true },
        errors: { ...prev.errors, createNetwork: null },
      }));

      try {
        // ensure network name is at least 6 characters
        if (params.networkName.length < 6) {
          const err = new Error("Network name must be at least 6 characters.");
          setState((prev) => ({
            ...prev,
            errors: { ...prev.errors, createNetwork: err.message },
          }));
          return { error: { message: err.message } };
        }

        // ensure terms accepted
        if (!params.terms) {
          const err = new Error("Terms must be accepted to create a network.");
          setState((prev) => ({
            ...prev,
            errors: { ...prev.errors, createNetwork: err.message },
          }));
          return { error: { message: err.message } };
        }

        const requestParams: NetworkCreateArgs = {
          terms: params.terms,
          guest_mode: false, // not allowing guest mode on web
        };

        if (state.userAuth && params.password && params.password.length > 11) {
          requestParams.user_auth = state.userAuth;
          requestParams.password = params.password;
        }

        // todo - handle SSO case
        // todo - handle solana wallet case

        const result = await api.networkCreate(requestParams);
        if (result.error) {
          const err = new Error(result.error.message);

          setState((prev) => ({
            ...prev,
            errors: { ...prev.errors, createNetwork: err.message },
          }));

          return result;
        }
        return result;
      } catch (err) {
        setState((prev) => ({
          ...prev,
          errors: { ...prev.errors, createNetwork: (err as Error).message },
        }));

        throw err;
      } finally {
        setState((prev) => ({
          ...prev,
          loading: { ...prev.loading, creatingNetwork: false },
        }));
      }
    },
    [api],
  );

  const value: AuthFlowContextValue = {
    state,
    setUserAuth,
    // setState,
    // updateState,
    // networkCheckState,
    checkUserAuth,
    loginWithPassword,
    createNetwork,
    // loginWithGoogle,
    // loginWithPassword,
    // createNetwork,
    // checkNetworkName,
    // reset,
    // clearError,
  };

  return (
    <AuthFlowContext.Provider value={value}>
      {children}
    </AuthFlowContext.Provider>
  );
}

export function useAuthFlow(): AuthFlowContextValue {
  const context = useContext(AuthFlowContext);
  if (!context) {
    throw new Error("useAuthFlow must be used within AuthFlowProvider");
  }
  return context;
}

/**
 * Auth context
 */
// import { storage, STORAGE_KEYS } from "./storage";

/**
 * Auth context
 */

export interface StorageAdapter {
  getItem: (key: string) => Promise<string | null>;
  setItem: (key: string, value: string) => Promise<void>;
  removeItem: (key: string) => Promise<void>;
}

interface AuthContextValue {
  token: string | null;
  networkName: string | null;
  isLoading: boolean;
  isAuthenticated: boolean;
  setAuth: (jwt: string, networkName?: string) => Promise<void>;
  clearAuth: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

interface AuthProviderProps {
  children: ReactNode;
  storage?: StorageAdapter; // Optional storage adapter
  onAuthChange?: (jwt: string | null, networkName: string | null) => void; // Optional callback
}

export function AuthProvider({
  children,
  storage: storageAdapter,
  onAuthChange,
}: AuthProviderProps) {
  const [token, setToken] = useState<string | null>(null);
  const [networkName, setNetworkName] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(true);

  // Load JWT from storage on mount (if storage adapter provided)
  useEffect(() => {
    const loadAuth = async () => {
      if (!storageAdapter) {
        setIsLoading(false);
        return;
      }

      try {
        const [jwt, name] = await Promise.all([
          storageAdapter.getItem("by_jwt"),
          storageAdapter.getItem("network_name"),
        ]);

        if (jwt) {
          setToken(jwt);
          setNetworkName(name);
        }
      } catch (error) {
        console.error("Error loading auth from storage:", error);
      } finally {
        setIsLoading(false);
      }
    };

    loadAuth();
  }, [storageAdapter]);

  const setAuth = useCallback(
    async (jwt: string, name?: string) => {
      try {
        setToken(jwt);

        if (name) {
          setNetworkName(name);
        }

        // Persist if storage adapter provided
        if (storageAdapter) {
          await storageAdapter.setItem("by_jwt", jwt);
          if (name) {
            await storageAdapter.setItem("network_name", name);
          }
        }

        // Call optional change callback
        onAuthChange?.(jwt, name ?? null);
      } catch (error) {
        console.error("Error saving auth:", error);
        throw error;
      }
    },
    [storageAdapter, onAuthChange],
  );

  const clearAuth = useCallback(async () => {
    try {
      setToken(null);
      setNetworkName(null);

      // Clear from storage if adapter provided
      if (storageAdapter) {
        await Promise.all([
          storageAdapter.removeItem("by_jwt"),
          storageAdapter.removeItem("network_name"),
        ]);
      }

      // Call optional change callback
      onAuthChange?.(null, null);
    } catch (error) {
      console.error("Error clearing auth:", error);
      throw error;
    }
  }, [storageAdapter, onAuthChange]);

  const value: AuthContextValue = {
    token,
    networkName,
    isLoading,
    isAuthenticated: !!token,
    setAuth,
    clearAuth,
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext);
  if (!context) {
    throw new Error("useAuth must be used within AuthProvider");
  }
  return context;
}
