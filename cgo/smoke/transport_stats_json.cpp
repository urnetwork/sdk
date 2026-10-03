// Verify the generated desktop ABI preserves live H1 selection and accepts an
// older SDK's JSON (absent fields mean no observed H1+ connection).
#include "urnetwork_sdk.hpp"

#include <cassert>
#include <cstdio>

int main() {
  using Json = nlohmann::json;
  const Json current = {{"TransportType", "h1"}, {"H1WebSocketConnectionCount", 2},
                        {"H1PlusConnectionCount", 3}, {"Enabled", true}};
  const auto share = current.get<urnet::TransportShare>();
  assert(share.TransportType == urnet::TransportTypeH1);
  assert(share.H1WebSocketConnectionCount == 2 && share.H1PlusConnectionCount == 3);
  const Json encoded = share;
  assert(encoded.at("H1PlusConnectionCount") == 3);
  assert(encoded.at("H1WebSocketConnectionCount") == 2);
  const auto packetStats = current.get<urnet::TransportPacketStats>();
  const Json packetEncoded = packetStats;
  assert(packetEncoded.at("H1PlusConnectionCount") == 3);
  assert(packetEncoded.at("H1WebSocketConnectionCount") == 2);
  const Json legacy = {{"TransportType", "h1"}, {"Percent", 100}, {"Used", true}};
  const auto oldShare = legacy.get<urnet::TransportShare>();
  assert(oldShare.Percent == 100 && oldShare.Used);
  assert(oldShare.H1PlusConnectionCount == 0 && oldShare.H1WebSocketConnectionCount == 0);
  const auto oldStats = legacy.get<urnet::TransportPacketStats>();
  assert(oldStats.H1PlusConnectionCount == 0 && oldStats.H1WebSocketConnectionCount == 0);
  std::puts("transport stats JSON round-trip and legacy defaults passed");
}
