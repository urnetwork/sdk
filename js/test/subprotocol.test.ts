import {test} from "node:test";
import assert from "node:assert/strict";
import {attachSubprotocolAPI} from "../src/subprotocol.ts";

const deferred = <T>() => { let resolve!: (v:T)=>void, reject!: (e:unknown)=>void; const promise = new Promise<T>((a,b)=>{resolve=a;reject=b}); return {promise,resolve,reject}; };
test("subprotocol callbacks retain each copied frame, stop on unsubscribe, and release once", async () => {
  const reads = [deferred<any>(), deferred<any>()]; let next = 0, releases = 0;
  const device = attachSubprotocolAPI({subprotocolOperation: async (op:string) => {
    if (op === "open") return {id: 5};
    if (op === "receive") return reads[next++].promise;
    if (op === "release") { releases++; reads[1].reject(new Error("closed")); }
  }});
  const seen:any[]=[];
  const sub = await device.enableSubprotocol(4096, message => {seen.push(message)});
  const bytes = Uint8Array.of(0,255,8);
  reads[0].resolve({sourceClientId:"peer", bytes});
  await new Promise(resolve=>setImmediate(resolve));
  bytes.fill(9);
  assert.deepEqual(seen, [{subprotocolId:4096,sourceClientId:"peer",bytes:Uint8Array.of(0,255,8)}]);
  await sub.close(); await sub.close(); await sub.closed;
  assert.equal(releases,1);
  await assert.rejects(sub.send("peer",bytes),/closed/);
});
test("subprotocol send snapshots bytes and query distinguishes missing from empty support", async () => {
  const read=deferred<any>(); const calls:any[]=[]; let result:any=null;
  const device=attachSubprotocolAPI({subprotocolOperation:async(op:string,_id:number,arg:unknown)=>{
    if(op==="open")return {id:1}; if(op==="receive")return read.promise;
    if(op==="release"){read.reject(new Error("closed"));return;}
    calls.push([op,arg]); return result;
  }});
  const sub=await device.enableSubprotocol(4096,()=>{});
  const input=Uint8Array.of(1,0,255); result=true;
  const sent=sub.send("destination",input);input.fill(0);assert.equal(await sent,true);
  assert.deepEqual(calls[0], ["send",{destinationClientId:"destination",bytes:Uint8Array.of(1,0,255)}]);
  result=null;assert.equal(await sub.querySubprotocols("destination",100),null);
  result=[];assert.deepEqual(await sub.querySubprotocols("destination"),[]);
  await assert.rejects(sub.querySubprotocols("destination",0),RangeError);
  await sub.close();await sub.closed;
});
test("subprotocol transport failure rejects closed and cleans native registration", async () => {
  let released=0;
  const device=attachSubprotocolAPI({subprotocolOperation:async(op:string)=>{
    if(op==="open")return {id:1};if(op==="receive")throw new Error("RPC disconnected");released++;
  }});
  const sub=await device.enableSubprotocol(4096,()=>{});
  await assert.rejects(sub.closed,/RPC disconnected/);assert.equal(released,1);
  await assert.rejects(device.enableSubprotocol(1,()=>{}),RangeError);
});
test("listener errors reject the subscription instead of continuing with dropped messages",async()=>{
  let released=0;
  const device=attachSubprotocolAPI({subprotocolOperation:async(op:string)=>{
    if(op==="open")return {id:1};if(op==="receive")return {sourceClientId:"peer",bytes:Uint8Array.of(1)};released++;
  }});
  const sub=await device.enableSubprotocol(4096,()=>{throw new Error("application decode failure")});
  await assert.rejects(sub.closed,/application decode failure/);assert.equal(released,1);
});
