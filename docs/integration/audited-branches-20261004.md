# Audited branch integration, 2026-10-04

The integration begins at origin/main e0db83787d16f4adb45287e3a342f23a70f4fc55.
Older branch parents are retained in merge history; this does not select an old
mobile build or change the current dependency graph.

- `cloud-proxy2` (`d291999c`): merged its resident-ID reconnect design notes.
  The pseudocode remains inside the existing block comment in `ProxyDevice.run`;
  runtime behavior is unchanged.
- `set-dir-log-after-init` (`f57af1f5`): its cleanup-after-directory-selection
  behavior is retained by current `setLogDirWithRoot`, including the newer
  successful-directory/root publication lock, per-process retention, 16 MiB
  log limit, and platform-specific stderr handling. The historical glog module
  replacement experiment and unconditional stdout redirection are superseded
  by the current dependency and platform implementations. The three merge
  conflicts are resolved to those current implementations, not the old experiment.
- `device-remote-fixes` (`3cf843d7`): stable patch ID
  `56f2b105292c7d2bb8fa8ce0f83ef691ae084bb3` exactly equals Main ancestor
  `86024736`. Retain current RPC ownership, current tests and current build
  layout when the old patch conflicts; no duplicate older implementation.
- `fix/locations-request-lifecycle` and `fix/picker-contract-status-rpc-lock`:
  all nonmerge patches are already equivalent on Main; merge ancestry only.
