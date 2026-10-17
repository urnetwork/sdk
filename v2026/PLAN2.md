
In this plan we will implement the subbed methods in the Device interface.


Design Goals


1. Implement the Device interface in DeviceLocal and DeviceRemote, including the RPC communcation.
2. The DeviceLocal implementation will need to thread into the connect package. We will implement all parts except the NetworkPeers part, which we will stub in connect/Client. For DeviceLocals with providers disabled (no provider client), there will be no network peers. NetworkPeers will be a future implementation. 
3. The BlockActionOverrides need to feed into the multi client. The multi client should surface all egress block and local decisions in an epoch that periodically gets fired back to the listeners. The epoch size should be configuable in settings. The goal is for all routing decisions to be surfaced to the listener. We should have a new BlockActionViewController that stores a time window of recent routing decisions, gated by a time window setting.
4. The multi client should group block decisions using an association matrix (ip_assoc.go) so that an IpPath gets matches with BlockActionOverride rules based on association not direct match. This is important because users will typically block a single host but want to block all the associated IpPaths. The implementation of ip_assoc.go is part of this plan.
5. Contract and companion contract stats will need to be threaded into SendSequence and ReceiveSequence (where the contracts are used), and joined in the DeviceLocal into paired contact and companion contract. The transfer keeps contract and companion contract separate, but they can be paired with the peer client id. The listeners of contract updates should again use an epoch, where the events get queued for a configurable epoch time and emitted at the end. Because there are so many packets we do not want to emit contract updates per packet.
6. Generally the default event epoch time should be 1s throughout.
7. There should be a ContractViewController that keeps a time series of total egress and ingress traffic, as a live throughput tracker. The contract window is only for the client traffic not provider traffic. We are currently not showing visibility into the provider contracts. As a next step we would want to add parallel insight for the provider traffic.
8. Block override actions affect both security rules (block) and local routing rules. The overrides, when set, should take precendent over the default decision in the multi client.

Implementation Goals

1. Focus on performance and memory efficiency.
2. Add tests in connect and sdk for the new surface area.
3. Make minimal and elegant changes and avoid introducing too many abstractions.

