package sdk

import "github.com/urnetwork/connect/v2026"

func applyH1ConnectionStats(stats *PacketStats, connections connect.H1ConnectionStatsSnapshot) {
	if stats == nil || stats.TransportStats == nil {
		return
	}
	for _, carrier := range stats.TransportStats.getAll() {
		if carrier != nil && carrier.TransportType == TransportTypeH1 {
			carrier.H1WebSocketConnectionCount = connections.WebSocketConnectionCount
			carrier.H1PlusConnectionCount = connections.H1PlusConnectionCount
			return
		}
	}
}

func h1ConnectionStatsFromPacketStats(stats *PacketStats) connect.H1ConnectionStatsSnapshot {
	if stats != nil && stats.TransportStats != nil {
		for _, carrier := range stats.TransportStats.getAll() {
			if carrier != nil && carrier.TransportType == TransportTypeH1 {
				return connect.H1ConnectionStatsSnapshot{
					WebSocketConnectionCount: carrier.H1WebSocketConnectionCount,
					H1PlusConnectionCount:    carrier.H1PlusConnectionCount,
				}
			}
		}
	}
	return connect.H1ConnectionStatsSnapshot{}
}
