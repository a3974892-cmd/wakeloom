"""Tiny auth proxy in front of ComfyUI for ComfyWorkbench local mode.

The cloudflared quick-tunnel URL is public, so every request (HTTP and WebSocket) must carry the
per-session secret in the X-WB-Key header (the workbench adds it); anything else gets 403.
"""
import asyncio
import hmac
import os

import aiohttp
from aiohttp import WSMsgType, web

KEY = os.environ.get("WB_KEY", "")
UP = "http://" + os.environ.get("WB_UPSTREAM", "127.0.0.1:8188")
PORT = int(os.environ.get("WB_PORT", "8189"))
HOP = {"host", "x-wb-key", "content-length", "transfer-encoding", "connection", "upgrade", "keep-alive",
       "proxy-authorization", "te", "trailer", "sec-websocket-key", "sec-websocket-version",
       "sec-websocket-extensions", "sec-websocket-protocol"}
session: aiohttp.ClientSession | None = None


def allowed(req: web.Request) -> bool:
    return bool(KEY) and hmac.compare_digest(req.headers.get("X-WB-Key", ""), KEY)


async def handle(req: web.Request) -> web.StreamResponse:
    if not allowed(req):
        return web.Response(status=403, text="forbidden")
    url = UP + str(req.rel_url)
    headers = {k: v for k, v in req.headers.items() if k.lower() not in HOP}
    if req.headers.get("Upgrade", "").lower() == "websocket":
        ws = web.WebSocketResponse(max_msg_size=0, autoping=True)
        await ws.prepare(req)
        try:
            async with session.ws_connect(url, headers=headers, max_msg_size=0) as up:
                async def pump(src, dst):
                    async for m in src:
                        if m.type == WSMsgType.TEXT:
                            await dst.send_str(m.data)
                        elif m.type == WSMsgType.BINARY:
                            await dst.send_bytes(m.data)
                        else:
                            break
                t1 = asyncio.create_task(pump(ws, up))
                t2 = asyncio.create_task(pump(up, ws))
                done, pending = await asyncio.wait({t1, t2}, return_when=asyncio.FIRST_COMPLETED)
                for t in pending:
                    t.cancel()
        except aiohttp.ClientError:
            pass
        await ws.close()
        return ws
    body = await req.read()
    try:
        async with session.request(req.method, url, headers=headers, data=body or None,
                                   allow_redirects=False) as r:
            out = web.StreamResponse(status=r.status, reason=r.reason)
            for k, v in r.headers.items():
                if k.lower() not in HOP:
                    out.headers.add(k, v)
            await out.prepare(req)
            async for chunk in r.content.iter_chunked(1 << 16):
                await out.write(chunk)
            await out.write_eof()
            return out
    except aiohttp.ClientError as e:
        return web.Response(status=502, text=f"ComfyUI unreachable: {e}")


async def main():
    global session
    session = aiohttp.ClientSession(auto_decompress=False, timeout=aiohttp.ClientTimeout(total=None, sock_connect=10))
    app = web.Application(client_max_size=1 << 30)
    app.router.add_route("*", "/{tail:.*}", handle)
    runner = web.AppRunner(app)
    await runner.setup()
    await web.TCPSite(runner, "127.0.0.1", PORT).start()
    await asyncio.Event().wait()


if __name__ == "__main__":
    asyncio.run(main())
