import { useCallback, useEffect, useRef, useState } from "react";
import { useAPI } from "../context";
import {
  ConnectLocation,
  FilteredLocations,
  FindLocationsArgs,
  FindLocationsResult,
} from "../../generated";

const initialFilteredLocations: FilteredLocations = {
  best_matches: [],
  promoted: [],
  countries: [],
  cities: [],
  regions: [],
  devices: [],
};

export function useProviderList() {
  const api = useAPI();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const [filteredLocations, setFilteredLocations] = useState<FilteredLocations>(
    initialFilteredLocations,
  );
  const [query, setQuery] = useState("");

  const debounceMs = 500;
  const timeoutRef = useRef<NodeJS.Timeout | null>(null);

  const filterLocations = (result: FindLocationsResult): FilteredLocations => {
    const bestMatch: ConnectLocation[] = [];
    const promoted: ConnectLocation[] = [];
    const countries: ConnectLocation[] = [];
    const cities: ConnectLocation[] = [];
    const regions: ConnectLocation[] = [];
    const devices: ConnectLocation[] = [];

    // Process groups
    if (result.groups) {
      for (const groupResult of result.groups) {
        const location: ConnectLocation = {
          connect_location_id: {
            location_group_id: groupResult.location_group_id,
          },
          name: groupResult.name,
          provider_count: groupResult.provider_count,
          promoted: groupResult.promoted,
          match_distance: groupResult.match_distance,
          stable: false,
          strong_privacy: false,
        };

        if (groupResult.match_distance === 0 && query !== "") {
          bestMatch.push(location);
        } else if (groupResult.promoted) {
          promoted.push(location);
        }
      }
    }

    // Process locations
    if (result.locations) {
      for (const locationResult of result.locations) {
        const location: ConnectLocation = {
          connect_location_id: {
            location_id: locationResult.location_id,
          },
          location_type: locationResult.location_type,
          name: locationResult.name,
          city: locationResult.city,
          region: locationResult.region,
          country: locationResult.country,
          country_code: locationResult.country_code,
          city_location_id: locationResult.city_location_id,
          region_location_id: locationResult.region_location_id,
          country_location_id: locationResult.country_location_id,
          provider_count: locationResult.provider_count,
          match_distance: locationResult.match_distance,
          stable: locationResult.stable,
          strong_privacy: locationResult.strong_privacy,
        };

        if (location.match_distance === 0 && query !== "") {
          bestMatch.push(location);
        } else {
          if (location.location_type === "country") {
            countries.push(location);
          }

          // only show cities when searching
          if (location.location_type === "city" && query !== "") {
            cities.push(location);
          }

          // only show regions when searching
          if (location.location_type === "region" && query !== "") {
            regions.push(location);
          }
        }
      }
    }

    // Process devices
    if (result.devices) {
      for (const locationDeviceResult of result.devices) {
        const location: ConnectLocation = {
          connect_location_id: {
            client_id: locationDeviceResult.client_id,
          },
          name: locationDeviceResult.device_name,
          stable: false,
          strong_privacy: false,
        };
        devices.push(location);
      }
    }

    // Comparison function for sorting
    const cmpConnectLocations = (
      a: ConnectLocation,
      b: ConnectLocation,
    ): number => {
      // Match distance priority (0 or 1 are considered close matches)
      const aCloseMatch = (a.match_distance ?? 0) <= 1;
      const bCloseMatch = (b.match_distance ?? 0) <= 1;
      if (aCloseMatch !== bCloseMatch) {
        return aCloseMatch ? -1 : 1;
      }

      // Provider count descending
      const aProviderCount = a.provider_count ?? 0;
      const bProviderCount = b.provider_count ?? 0;
      if (aProviderCount !== bProviderCount) {
        return bProviderCount - aProviderCount;
      }

      // Name ascending
      const aName = a.name ?? "";
      const bName = b.name ?? "";
      if (aName !== bName) {
        return aName.localeCompare(bName);
      }

      // Compare location IDs as a final tiebreaker
      const aId = JSON.stringify(a.connect_location_id);
      const bId = JSON.stringify(b.connect_location_id);
      return aId.localeCompare(bId);
    };

    // Sort all arrays
    bestMatch.sort(cmpConnectLocations);
    promoted.sort(cmpConnectLocations);
    countries.sort(cmpConnectLocations);
    cities.sort(cmpConnectLocations);
    regions.sort(cmpConnectLocations);

    return {
      best_matches: bestMatch,
      promoted: promoted,
      countries: countries,
      cities: cities,
      regions: regions,
      devices: devices,
    };
  };

  const getProviders = useCallback(
    async (search: string): Promise<void> => {
      setLoading(true);
      setError(null);

      try {
        let result;
        if (search.length <= 0) {
          result = await api.networkProviderLocations();
        } else {
          const params: FindLocationsArgs = {
            query: search,
          };
          result = await api.searchProviderLocations(params);
        }

        const filteredLocations = filterLocations(result);
        setFilteredLocations(filteredLocations);
      } catch (error) {
        setError(error as Error);
        setFilteredLocations(initialFilteredLocations);
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  // Trigger debounced search whenever query changes
  useEffect(() => {
    // Clear existing timeout
    if (timeoutRef.current) {
      clearTimeout(timeoutRef.current);
    }

    // Set new timeout
    timeoutRef.current = setTimeout(() => {
      getProviders(query);
    }, debounceMs);

    // Cleanup function
    return () => {
      if (timeoutRef.current) {
        clearTimeout(timeoutRef.current);
      }
    };
  }, [query, getProviders]);

  return {
    loading,
    error,
    filteredLocations,
    query,
    setQuery,
  };
}
