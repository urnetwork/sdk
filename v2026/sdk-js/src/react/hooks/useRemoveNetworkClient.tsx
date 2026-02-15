import { useCallback, useState } from "react";
import { useAPI, useAuth } from "../context";
import { tokenRequiredError } from "./constants";

export function useRemoveNetworkClient() {
  const api = useAPI();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const { token } = useAuth();

  const removeNetworkClient = useCallback(
    async (clientId: string) => {
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
        const result = await api.removeNetworkClient(
          {
            client_id: clientId,
          },
          token,
        );

        return result;
      } catch (err) {
        setError(err as Error);
        return {
          error: {
            message:
              err instanceof Error
                ? err.message
                : "Remove network client failed",
          },
        };
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  return {
    loading,
    error,
    removeNetworkClient,
  };
}
