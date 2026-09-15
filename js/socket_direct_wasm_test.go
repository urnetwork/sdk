//go:build js

package main

import (
	"context"
	"io"
	"net"
	"syscall/js"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

type directWasmDevice struct {
	sdk.Device
	tun *connect.Tun
	ctx context.Context
}

func (d *directWasmDevice) Ctx() context.Context { return d.ctx }
func (d *directWasmDevice) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.tun.DialContext(ctx, network, address)
}

// Import the production TypeScript in Node's WASM runner and connect its Web
// Streams to real TCP/UDP packets. No native destination socket is opened.
func TestDirectSocketsWasmTCPAndUDP(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client, err := connect.CreateTun(ctx, connect.DefaultTunSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := connect.CreateTun(ctx, connect.DefaultTunSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	pump := func(from, to *connect.Tun) {
		for {
			packet, err := from.Read()
			if err != nil {
				return
			}
			_, _ = to.Write(packet)
			connect.MessagePoolReturn(packet)
		}
	}
	go pump(client, peer)
	go pump(peer, client)
	m := map[string]any{}
	handles := jsBindSocketDevice(&directWasmDevice{tun: client, ctx: ctx}, m)
	defer handles.close()
	defer m["socketOperation"].(js.Func).Release()
	run := js.Global().Call("eval", `(async (bridge, host, port, udp) => {
  const {attachSocketAPI} = await import('file://' + process.cwd() + '/src/socket.ts');
  const {TCPSocket, UDPSocket} = attachSocketAPI(bridge).directSockets;
  const socket = udp ? new UDPSocket({remoteAddress: host, remotePort: port}) : new TCPSocket(host, port);
  const info = await socket.opened;
  if (info.remoteAddress !== host || info.remotePort !== port) throw new Error('incorrect opened address');
  const writer = info.writable.getWriter();
  const reader = udp ? info.readable.getReader() : info.readable.getReader({mode: 'byob'});
  try {
    if (udp) {
      await writer.write({data: new ArrayBuffer(0)});
      const empty = await reader.read();
      if (empty.done || empty.value.data.length !== 0 || 'remoteAddress' in empty.value) throw new Error('empty connected datagram');
      await writer.write({data: Uint8Array.of(1, 2, 3)});
      const reply = await reader.read();
      if (String(reply.value.data) !== '1,2,3') throw new Error('UDP payload');
    } else {
      const payload = new Uint8Array(140000).fill(37);
      await writer.write(new DataView(payload.buffer));
      await writer.close();
      let total = 0;
      while (true) {
        const reply = await reader.read(new Uint8Array(4093));
        if (reply.done) break;
        if (reply.value.some(value => value !== 37)) throw new Error('TCP byte corruption');
        total += reply.value.length;
      }
      if (total !== payload.length) throw new Error('TCP byte count ' + total);
    }
  } finally {
    reader.releaseLock(); writer.releaseLock();
    await socket.close(); await socket.closed;
  }
  return true;
})`)
	for _, address := range peer.LocalAddresses() {
		for _, udp := range []bool{false, true} {
			name := "tcp"
			if udp {
				name = "udp"
			}
			t.Run(name+"/"+address.String(), func(t *testing.T) {
				ip := net.IP(address.AsSlice())
				port := 8123
				if udp {
					pc, err := peer.ListenUDP(&net.UDPAddr{IP: ip, Port: port})
					if err != nil {
						t.Fatal(err)
					}
					defer pc.Close()
					go func() {
						for {
							buf := make([]byte, 65535)
							n, remote, err := pc.ReadFrom(buf)
							if err != nil {
								return
							}
							_, _ = pc.WriteTo(buf[:n], remote)
						}
					}()
				} else {
					ln, err := peer.ListenTCP(&net.TCPAddr{IP: ip, Port: port})
					if err != nil {
						t.Fatal(err)
					}
					defer ln.Close()
					go func() {
						conn, err := ln.Accept()
						if err == nil {
							defer conn.Close()
							_, _ = io.Copy(conn, conn)
						}
					}()
				}
				if _, err := awaitSocketPromise(t, run.Invoke(m, address.String(), port, udp)); err != nil {
					t.Fatal(err)
				}
				handles.mu.Lock()
				defer handles.mu.Unlock()
				if len(handles.values) != 0 || len(handles.done) != 0 {
					t.Fatal("Direct Socket leaked its WASM handle")
				}
			})
		}
	}
}
