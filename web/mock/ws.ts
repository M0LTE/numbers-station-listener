// A minimal RFC 6455 server side, enough for the mock spectrum socket:
// text and binary sends, close, ping/pong. Dev-only; never bundled.

import { createHash } from "node:crypto";
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";

const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

export interface MockSocket {
  sendText(s: string): void;
  sendBinary(b: Uint8Array): void;
  ping(): void;
  close(code?: number, reason?: string): void;
  readonly open: boolean;
  lastPong: number;
  onClose: (() => void) | null;
}

export function acceptUpgrade(req: IncomingMessage, socket: Duplex, head: Buffer): MockSocket | null {
  const key = req.headers["sec-websocket-key"];
  if (typeof key !== "string") {
    socket.end("HTTP/1.1 400 Bad Request\r\n\r\n");
    return null;
  }
  const accept = createHash("sha1").update(key + GUID).digest("base64");
  socket.write(
    "HTTP/1.1 101 Switching Protocols\r\n" +
      "Upgrade: websocket\r\n" +
      "Connection: Upgrade\r\n" +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
  );

  let open = true;
  let buf: Buffer = head.length ? Buffer.from(head) : Buffer.alloc(0);

  const frame = (opcode: number, payload: Uint8Array): void => {
    if (!open) return;
    const len = payload.length;
    let header: Buffer;
    if (len < 126) {
      header = Buffer.from([0x80 | opcode, len]);
    } else if (len < 65536) {
      header = Buffer.alloc(4);
      header[0] = 0x80 | opcode;
      header[1] = 126;
      header.writeUInt16BE(len, 2);
    } else {
      header = Buffer.alloc(10);
      header[0] = 0x80 | opcode;
      header[1] = 127;
      header.writeBigUInt64BE(BigInt(len), 2);
    }
    socket.write(Buffer.concat([header, payload]));
  };

  const finish = (): void => {
    if (!open) return;
    open = false;
    ms.onClose?.();
  };

  const ms: MockSocket = {
    sendText: (s) => frame(0x1, Buffer.from(s, "utf8")),
    sendBinary: (b) => frame(0x2, b),
    ping: () => frame(0x9, Buffer.alloc(0)),
    close: (code = 1000, reason = "") => {
      if (!open) return;
      const r = Buffer.from(reason, "utf8");
      const p = Buffer.alloc(2 + r.length);
      p.writeUInt16BE(code, 0);
      r.copy(p, 2);
      frame(0x8, p);
      socket.end();
      finish();
    },
    get open() {
      return open;
    },
    lastPong: Date.now(),
    onClose: null,
  };

  const parse = (): void => {
    for (;;) {
      if (buf.length < 2) return;
      const opcode = buf[0] & 0x0f;
      const masked = (buf[1] & 0x80) !== 0;
      let len = buf[1] & 0x7f;
      let off = 2;
      if (len === 126) {
        if (buf.length < 4) return;
        len = buf.readUInt16BE(2);
        off = 4;
      } else if (len === 127) {
        if (buf.length < 10) return;
        len = Number(buf.readBigUInt64BE(2));
        off = 10;
      }
      const maskOff = off;
      if (masked) off += 4;
      if (buf.length < off + len) return;
      const payload = Buffer.from(buf.subarray(off, off + len));
      if (masked) {
        for (let i = 0; i < payload.length; i++) payload[i] ^= buf[maskOff + (i & 3)];
      }
      buf = buf.subarray(off + len);
      if (opcode === 0x8) {
        if (open) {
          frame(0x8, payload.subarray(0, 2));
          socket.end();
        }
        finish();
        return;
      }
      if (opcode === 0x9) frame(0xa, payload);
      if (opcode === 0xa) ms.lastPong = Date.now();
    }
  };

  socket.on("data", (d: Buffer) => {
    buf = buf.length ? Buffer.concat([buf, d]) : d;
    parse();
  });
  socket.on("close", finish);
  socket.on("error", finish);
  if (buf.length) parse();
  return ms;
}
