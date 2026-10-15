# xdev Browser (Chrome extension)

MV3 extension that lets the xdev agent drive **your existing Chrome tabs**
(cookies, logins, open pages) without starting a separate debugging profile.

## How it works

```
xdev browser tool  --CDP-->  127.0.0.1:9224 (native host)
                                  |
                          native messaging
                                  |
                         this extension  --chrome.debugger-->  your tabs
```

The native host speaks Chrome's DevTools HTTP discovery (`/json/list`) and a
per-tab websocket, so the existing `browser` tool needs no new ops. The
extension attaches the Chrome debugger API to the real profile.

## Install

```bash
xdev chrome install
```

That command:

1. Copies this folder to `~/.xdev/agent/chrome/extension`
2. Writes the native-messaging host wrapper + Chrome registry manifest
3. Prints chrome://extensions steps (Load unpacked → that folder)

Then in a session, either rely on the default chrome bridge port or set:

```yaml
browser:
  cdpUrl: http://127.0.0.1:9224
  autolaunch: false
```

## Permissions

- `debugger` — CDP commands on tabs you use with xdev
- `tabs` — list open pages for `/json/list`
- `nativeMessaging` — talk to the local `xdev --chrome-native-host` process

The extension never sends page content off-machine. Traffic stays on loopback
between Chrome and the xdev binary.
