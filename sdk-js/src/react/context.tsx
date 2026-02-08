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
 * Auth context
 */

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
