// A country's continent (lag.dev/DRIFTTEST.md V11), as the server's
// model.ContinentCodeForCountry has it (server model/continent_codes.go,
// 084a04baa): an ISO 3166-1 alpha-2 code, in any case, to one of GeoLite2's
// and Route 53's seven continent codes, the vocabulary defaultK takes. The
// assignments follow the usual geolocation database convention, not the UN
// M49 geoscheme: Cyprus and Russia are EU, Turkey and the Transcaucasus AS,
// the Caribbean NA, and the territories around Antarctica AN. Pinned to the
// Go by test/lag/drift-continents.json.

/** The seven continent codes, in the server's order. */
export const continentCodes: readonly string[] = Object.freeze(["AF", "AN", "AS", "EU", "NA", "OC", "SA"]);

// every code the server's table has, lower case, to its continent
const countryContinentCodes: ReadonlyMap<string, string> = new Map<string, string>([
  ["ad", "EU"], ["ae", "AS"], ["af", "AS"], ["ag", "NA"], ["ai", "NA"], ["al", "EU"], ["am", "AS"], ["ao", "AF"],
  ["aq", "AN"], ["ar", "SA"], ["as", "OC"], ["at", "EU"], ["au", "OC"], ["aw", "NA"], ["ax", "EU"], ["az", "AS"],
  ["ba", "EU"], ["bb", "NA"], ["bd", "AS"], ["be", "EU"], ["bf", "AF"], ["bg", "EU"], ["bh", "AS"], ["bi", "AF"],
  ["bj", "AF"], ["bl", "NA"], ["bm", "NA"], ["bn", "AS"], ["bo", "SA"], ["bq", "NA"], ["br", "SA"], ["bs", "NA"],
  ["bt", "AS"], ["bv", "AN"], ["bw", "AF"], ["by", "EU"], ["bz", "NA"], ["ca", "NA"], ["cc", "AS"], ["cd", "AF"],
  ["cf", "AF"], ["cg", "AF"], ["ch", "EU"], ["ci", "AF"], ["ck", "OC"], ["cl", "SA"], ["cm", "AF"], ["cn", "AS"],
  ["co", "SA"], ["cr", "NA"], ["cu", "NA"], ["cv", "AF"], ["cw", "NA"], ["cx", "AS"], ["cy", "EU"], ["cz", "EU"],
  ["de", "EU"], ["dj", "AF"], ["dk", "EU"], ["dm", "NA"], ["do", "NA"], ["dz", "AF"], ["ec", "SA"], ["ee", "EU"],
  ["eg", "AF"], ["eh", "AF"], ["er", "AF"], ["es", "EU"], ["et", "AF"], ["fi", "EU"], ["fj", "OC"], ["fk", "SA"],
  ["fm", "OC"], ["fo", "EU"], ["fr", "EU"], ["ga", "AF"], ["gb", "EU"], ["gd", "NA"], ["ge", "AS"], ["gf", "SA"],
  ["gg", "EU"], ["gh", "AF"], ["gi", "EU"], ["gl", "NA"], ["gm", "AF"], ["gn", "AF"], ["gp", "NA"], ["gq", "AF"],
  ["gr", "EU"], ["gs", "AN"], ["gt", "NA"], ["gu", "OC"], ["gw", "AF"], ["gy", "SA"], ["hk", "AS"], ["hm", "AN"],
  ["hn", "NA"], ["hr", "EU"], ["ht", "NA"], ["hu", "EU"], ["id", "AS"], ["ie", "EU"], ["il", "AS"], ["im", "EU"],
  ["in", "AS"], ["io", "AS"], ["iq", "AS"], ["ir", "AS"], ["is", "EU"], ["it", "EU"], ["je", "EU"], ["jm", "NA"],
  ["jo", "AS"], ["jp", "AS"], ["ke", "AF"], ["kg", "AS"], ["kh", "AS"], ["ki", "OC"], ["km", "AF"], ["kn", "NA"],
  ["kp", "AS"], ["kr", "AS"], ["kw", "AS"], ["ky", "NA"], ["kz", "AS"], ["la", "AS"], ["lb", "AS"], ["lc", "NA"],
  ["li", "EU"], ["lk", "AS"], ["lr", "AF"], ["ls", "AF"], ["lt", "EU"], ["lu", "EU"], ["lv", "EU"], ["ly", "AF"],
  ["ma", "AF"], ["mc", "EU"], ["md", "EU"], ["me", "EU"], ["mf", "NA"], ["mg", "AF"], ["mh", "OC"], ["mk", "EU"],
  ["ml", "AF"], ["mm", "AS"], ["mn", "AS"], ["mo", "AS"], ["mp", "OC"], ["mq", "NA"], ["mr", "AF"], ["ms", "NA"],
  ["mt", "EU"], ["mu", "AF"], ["mv", "AS"], ["mw", "AF"], ["mx", "NA"], ["my", "AS"], ["mz", "AF"], ["na", "AF"],
  ["nc", "OC"], ["ne", "AF"], ["nf", "OC"], ["ng", "AF"], ["ni", "NA"], ["nl", "EU"], ["no", "EU"], ["np", "AS"],
  ["nr", "OC"], ["nu", "OC"], ["nz", "OC"], ["om", "AS"], ["pa", "NA"], ["pe", "SA"], ["pf", "OC"], ["pg", "OC"],
  ["ph", "AS"], ["pk", "AS"], ["pl", "EU"], ["pm", "NA"], ["pn", "OC"], ["pr", "NA"], ["ps", "AS"], ["pt", "EU"],
  ["pw", "OC"], ["py", "SA"], ["qa", "AS"], ["re", "AF"], ["ro", "EU"], ["rs", "EU"], ["ru", "EU"], ["rw", "AF"],
  ["sa", "AS"], ["sb", "OC"], ["sc", "AF"], ["sd", "AF"], ["se", "EU"], ["sg", "AS"], ["sh", "AF"], ["si", "EU"],
  ["sj", "EU"], ["sk", "EU"], ["sl", "AF"], ["sm", "EU"], ["sn", "AF"], ["so", "AF"], ["sr", "SA"], ["ss", "AF"],
  ["st", "AF"], ["sv", "NA"], ["sx", "NA"], ["sy", "AS"], ["sz", "AF"], ["tc", "NA"], ["td", "AF"], ["tf", "AN"],
  ["tg", "AF"], ["th", "AS"], ["tj", "AS"], ["tk", "OC"], ["tl", "AS"], ["tm", "AS"], ["tn", "AF"], ["to", "OC"],
  ["tr", "AS"], ["tt", "NA"], ["tv", "OC"], ["tw", "AS"], ["tz", "AF"], ["ua", "EU"], ["ug", "AF"], ["um", "OC"],
  ["us", "NA"], ["uy", "SA"], ["uz", "AS"], ["va", "EU"], ["vc", "NA"], ["ve", "SA"], ["vg", "NA"], ["vi", "NA"],
  ["vn", "AS"], ["vu", "OC"], ["wf", "OC"], ["ws", "OC"], ["xk", "EU"], ["ye", "AS"], ["yt", "AF"], ["za", "AF"],
  ["zm", "AF"], ["zw", "AF"],
]);

// Go's strings.TrimSpace: what unicode.IsSpace counts as space, at either end
const goSpace = "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const trimGoSpace = new RegExp(`^[${goSpace}]+|[${goSpace}]+$`, "g");

/**
 * The continent of a country: GeoLite2's code (AF, AN, AS, EU, NA, OC or
 * SA) for an ISO 3166-1 alpha-2 country code in any case, with space around
 * it trimmed, or "" when the code is empty or unknown, which defaultK takes
 * as the unknown continent. For a start whose answer lacks the run's k, and
 * for lag.dev's radar: defaultK(confidence, continentForCountry(code)).
 */
export function continentForCountry(countryCode: string): string {
  return countryContinentCodes.get(countryCode.replace(trimGoSpace, "").toLowerCase()) ?? "";
}
