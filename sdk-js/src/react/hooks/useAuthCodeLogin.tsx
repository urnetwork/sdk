import { useCallback, useState } from "react";
import { useAPI } from "../context";
import { AuthCodeLoginResult } from "../../generated";

export function useAuthCodeLogin() {
  const api = useAPI();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const authCodeLogin = useCallback(
    async (authCode: string): Promise<AuthCodeLoginResult> => {
      setLoading(true);
      setError(null);
      try {
        const result = await api.authCodeLogin({
          auth_code: authCode,
        });

        if (result.error) {
          const err = new Error(result.error.message);
          setError(err);
        }

        return result;
      } catch (err) {
        setError(err as Error);
        return {
          by_jwt: "",
          error: {
            message:
              err instanceof Error ? err.message : "Auth code login failed",
          },
        };
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  return { loading, error, authCodeLogin };
}
