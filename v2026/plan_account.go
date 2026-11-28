package sdk

// Which account holds the plan (support inbox 6546; also 7765, 6711, 706 and
// 7261): customers with several accounts paid on one and read the plan of
// another, and nothing on the plan surfaces said which account they were
// looking at. Every app now shows the account's name with the plan, and in the
// purchase confirmation, from this one rule, so every platform shows the same
// thing.
//
// js/src/utils/plan_account.ts is the TypeScript twin, for ur.io, and
// js/test/plan_account_vectors.json holds the vectors both replay.

import (
	"regexp"
	"strings"
)

// A legacy guest network's name: "g" and its network id, with or without the
// id's dashes. Nobody signs in with it.
var planAccountGuestNamePattern = regexp.MustCompile(`^g[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$`)

// The account name the plan surfaces show with the plan (the Account screen's
// plan, the Connect screen's plan card, the subscription screen) and in the
// purchase confirmation: the signed-in network's own name, trimmed, so a
// customer with several accounts sees which one the plan, Free or Pro, is on.
// networkName is the signed-in network's name (the sign-in token's
// network_name). The answer is "" for a guest network, whose name is an id
// nobody signs in with, whether the app knows it is a guest (guest) or the
// name says so, and when the name is empty: then the plan shows alone. It
// never names another network.
func PlanAccountName(networkName string, guest bool) string {
	name := strings.TrimSpace(networkName)
	if guest || planAccountGuestNamePattern.MatchString(name) {
		return ""
	}
	return name
}
