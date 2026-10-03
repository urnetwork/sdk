import { test } from "node:test";
import assert from "node:assert/strict";
import { attachSocketAPI, Conn } from "../src/socket.ts";

const deferred = <T>() => { let resolve!: (value: T) => void, reject!: (reason: unknown) => void; const promise = new Promise<T>((a,b)=>{resolve=a;reject=b}); return {promise,resolve,reject}; };
test("socket read distinguishes empty datagram, bytes plus EOF and EOF", async () => {
 const reads=[{data:new Uint8Array(0),eof:false},{data:Uint8Array.of(9),eof:true}];
 const bridge={socketOperation:async(op:string)=>op==="read"?reads.shift():{localAddr:"local",remoteAddr:"winner"}};
 const c=new Conn(bridge,{id:1},"udp");assert.deepEqual(await c.read(),new Uint8Array());assert.deepEqual(await c.read(),Uint8Array.of(9));assert.equal(await c.read(),null);assert.equal(c.remoteAddr,"winner");
});
test("TCP writes split large buffers, UDP writes retain message boundaries",async()=>{
 const sizes:number[]=[];const bridge={socketOperation:async(op:string,_id:number,arg:any)=>{if(op==="write"){sizes.push(arg.length);return arg.length}}};
 const tcp=new Conn(bridge,{id:1},"tcp");assert.equal(await tcp.write(new Uint8Array(140000)),140000);assert.deepEqual(sizes,[65535,65535,8930]);
 sizes.length=0;const udp=new Conn(bridge,{id:2},"udp");await udp.write(Uint8Array.of(1,2));await udp.write(new Uint8Array());assert.deepEqual(sizes,[2,0]);await assert.rejects(udp.write(new Uint8Array(65508)),RangeError);
});
test("write snapshots bytes and reports a short write",async()=>{
 const pending=deferred<number>();let captured!:Uint8Array;const c=new Conn({socketOperation:async(_o,_h,p)=>{captured=p as Uint8Array;return pending.promise}},{id:1},"tcp");const data=Uint8Array.of(3,4);const write=c.write(data);data[0]=9;await new Promise(resolve=>setImmediate(resolve));assert.equal(captured[0],3);pending.resolve(1);await assert.rejects(write,(e:any)=>e.bytesWritten===1);
});
test("absolute deadlines and close use the bridge; release is idempotent",async()=>{
 const calls:any[]=[];const c=new Conn({socketOperation:async(...a)=>{calls.push(a)}},{id:2},"tcp");await c.setReadDeadline(new Date(1000));await c.setDeadline(null);await c.close();await c.close();assert.deepEqual(calls,[["readDeadline",2,1000],["deadline",2,0],["release",2,null]]);await assert.rejects(c.write(Uint8Array.of(1)),/closed/);assert.throws(()=>c.setDeadline(NaN),RangeError);
});
test("dial passes TLS and cancellation options without invoking native browser sockets",async()=>{
 const calls:any[]=[];const d=attachSocketAPI({socketOperation:async(...args:any[])=>{calls.push(args);return {id:4}}});await d.dialTls("tcp","socket.test:443",{serverName:"socket.test"},{timeoutMillis:100});assert.equal(calls[0][0],"dialTls");assert.equal(calls[0][2].timeoutMillis,100);const ac=new AbortController();ac.abort();await assert.rejects(d.dial("udp","socket.test:9",{signal:ac.signal}));assert.equal(calls.length,1);
});
test("a dial result arriving after cancellation is released",async()=>{
 const pending=deferred<any>(),released:number[]=[];const d=attachSocketAPI({socketOperation:async(op:string,id:number)=>{if(op==="dial")return pending.promise;released.push(id)}});const ac=new AbortController();const p=d.dial("tcp","test:1",{signal:ac.signal});ac.abort();pending.resolve({id:8});await assert.rejects(p);assert.deepEqual(released,[8]);
});
test("socket streams apply backpressure and half-close the write side",async()=>{
 const gate=deferred<number>();const calls:string[]=[];const c=new Conn({socketOperation:async(op)=>{calls.push(op);if(op==="write")return gate.promise;return {data:new Uint8Array(),eof:true}}},{id:1},"tcp");assert.deepEqual(calls,[]);const writer=c.writable.getWriter();const write=writer.write(Uint8Array.of(1));await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(calls,["write"]);gate.resolve(1);await write;await writer.close();assert.equal(calls.at(-1),"closeWrite");const reader=c.readable.getReader();assert.equal((await reader.read()).done,true);
});
test("concurrent logical TCP writes do not interleave chunks", async () => {
 const order:number[]=[];
 const c=new Conn({socketOperation:async(_op,_id,data:any)=>{order.push(data[0]);await new Promise(resolve=>setImmediate(resolve));return data.length}},{id:1},"tcp");
 await Promise.all([c.write(new Uint8Array(140000).fill(1)),c.write(new Uint8Array(70000).fill(2))]);
 assert.deepEqual(order,[1,1,1,2,2]);
});
test("partial I/O retains bytes and exposes the error", async () => {
 const c=new Conn({socketOperation:async(op)=>op==="read"?{data:Uint8Array.of(8),error:"read timeout"}:{bytesWritten:2,error:"write timeout"}},{id:1},"tcp");
 assert.deepEqual(await c.read(),Uint8Array.of(8));await assert.rejects(c.read(),/read timeout/);
 await assert.rejects(c.write(Uint8Array.of(1,2,3)),(e:any)=>e.bytesWritten===2 && /write timeout/.test(e.message));
});
