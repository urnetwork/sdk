import { useCallback, useState } from "react";
import { useAPI, useAuth } from "../context";
import { AuthVerifyResult } from "../../generated";
import { tokenRequiredError } from "./constants";

/**
 * Used for verifying a email or phone number when creating a new network
 */
export function useVerifyUserAuth() {
  const api = useAPI();
  const { token } = useAuth();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const verifyUserAuth = useCallback(
    async (userAuth: string, code: string): Promise<AuthVerifyResult> => {
      if (!token) {
        const err = new Error(tokenRequiredError);
        setError(err);
        return {
          error: {
            message: err.message,
          },
        };
      }

      setLoading(true);
      setError(null);
      try {
        const result = await api.verifyUserAuth(
          {
            user_auth: userAuth,
            verify_code: code,
          },
          token,
        );

        return result;
      } catch (err) {
        setError(err as Error);
        return {
          error: {
            message: err instanceof Error ? err.message : "Verification failed",
          },
        };
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  return {
    verifyUserAuth,
    loading,
    error,
  };
}
