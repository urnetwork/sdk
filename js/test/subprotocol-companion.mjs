// Invoked by TestSubprotocolWasmCompanionRoundTrip with an isolated local fixture.
import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import {URNetwork} from "../dist/index.js";

const config = JSON.parse(await readFile(process.argv[2], "utf8"));
const sdk = await URNetwork.init();
const transport = {open(callbacks) {
  const ws = new WebSocket(config.url); ws.binaryType = "arraybuffer";
  const opened = () => callbacks.opened();
  const message = event => callbacks.message(new Uint8Array(event.data).slice());
  const closed = () => callbacks.closed("closed");
  ws.addEventListener("open", opened); ws.addEventListener("message", message);
  ws.addEventListener("close", closed); ws.addEventListener("error", closed);
  return {send(bytes) {ws.send(bytes.slice());}, close() {
    ws.removeEventListener("open", opened); ws.removeEventListener("message", message);
    ws.removeEventListener("close", closed); ws.removeEventListener("error", closed); ws.close();
  }};
}};
let device, sub, removePeers;
try {
  device = sdk.createExtensionDeviceRemote({...config, transport});
  let snapshotResolve;
  const snapshot = new Promise(resolve => {snapshotResolve = resolve;});
  // Browser listener registration must precede the first RPC sync.
  removePeers = device.addNetworkPeersChangeListener(peers => {if (peers !== null) snapshotResolve(peers);});
  for (let i = 0; !device.getRemoteConnected(); i++) {
    assert.equal(device.getSyncError(), "");
    assert.ok(i < 150, "DeviceRemote did not sync");
    await new Promise(resolve => setTimeout(resolve, 20));
  }
  assert.equal(device.getClientId(), config.clientId);
  assert.equal(device.getInstanceId(), config.instanceId);
  const initial = device.getNetworkPeers(); if (initial !== null) snapshotResolve(initial);
  let resolveAck, resolveText;
  const gotAck = new Promise(resolve => {resolveAck = resolve;});
  const gotText = new Promise(resolve => {resolveText = resolve;});
  const text = Uint8Array.from(Buffer.from("55524d530101000200000000000000016869", "hex"));
  sub = await device.enableSubprotocol(4096, async message => {
    assert.equal(message.sourceClientId, config.peer);
    const hex = Buffer.from(message.bytes).toString("hex");
    if (hex === "55524d53010200000000000000000001") resolveAck();
    else {
      assert.equal(hex, "55524d530101000200000000000000026869");
      assert.equal(await sub.send(config.peer, Uint8Array.from(Buffer.from("55524d53010200000000000000000002", "hex"))), true);
      resolveText();
    }
  });
  assert.ok((await sub.querySubprotocols(config.peer, 1000)).includes(4096));
  const sent = sub.send(config.peer, text); text.fill(0); assert.equal(await sent, true);
  const peers = await Promise.race([Promise.all([snapshot, gotAck, gotText]), sub.closed.then(() => {throw new Error("subscription closed");})]);
  assert.ok(Array.isArray(peers[0].connected));
  await sub.close(); await sub.closed;
  await assert.rejects(sub.send(config.peer, new Uint8Array()), /closed/);
  console.log("Packaged Node WASM exchanged exact URMS TEXT/ACK frames with native network peer; queried support, received live peers, and unsubscribed.");
} finally {
  if (sub) {await sub.close(); await sub.closed.catch(() => {});}
  removePeers?.(); device?.close(); sdk.close();
}
