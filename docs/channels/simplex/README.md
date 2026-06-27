> Back to [README](../../../README.md)

# SimpleX Chat

[SimpleX Chat](https://simplex.chat) is a privacy-focused messenger with no user
identifiers and no central server. Because of its end-to-end-encrypted protocol
there is no native Go SDK, so PicoClaw integrates the same way the official bots
do: by connecting as a WebSocket client to a locally-run `simplex-chat` CLI
server. That CLI process owns the SimpleX profile, keys and message queues;
PicoClaw only exchanges JSON commands and events with it (this mirrors how the
OneBot channel talks to an external bridge).

This channel supports **direct (1:1) messages**, **inbound and outbound file /
image attachments**, and **automatic acceptance of incoming contact requests**.
Group chats are not supported yet.

## Prerequisites: run the simplex-chat CLI

1. Install the `simplex-chat` CLI - see the
   [SimpleX CLI guide](https://simplex.chat/docs/cli.html). On first run it
   creates a local chat profile/database.
2. Start it as a WebSocket server (pick any free port):

   ```bash
   simplex-chat -p 5225
   ```

   This exposes the chat API at `ws://127.0.0.1:5225`. Keep this process running
   alongside PicoClaw, ideally on the same host so received files are reachable.

## Configuration

```json
{
  "channels": {
    "simplex": {
      "enabled": true,
      "type": "simplex",
      "allow_from": [],
      "settings": {
        "ws_url": "ws://127.0.0.1:5225",
        "reconnect_interval": 5,
        "auto_accept": true,
        "files_folder": ""
      }
    }
  }
}
```

| Field                | Type   | Required | Description                                                                                                    |
| -------------------- | ------ | -------- | -------------------------------------------------------------------------------------------------------------- |
| `enabled`            | bool   | Yes      | Whether to enable the SimpleX channel.                                                                          |
| `ws_url`             | string | No       | WebSocket URL of the `simplex-chat` CLI server. Defaults to `ws://127.0.0.1:5225`.                             |
| `reconnect_interval` | int    | No       | Seconds between reconnection attempts if the WebSocket drops. `0` disables reconnect (fail fast on startup).   |
| `auto_accept`        | bool   | No       | Automatically accept incoming contact requests so anyone with the bot's address can chat. Defaults to `true`.  |
| `files_folder`       | string | No       | Directory the CLI saves received files to. Set it to enable inbound images/files. Must be readable by PicoClaw. |
| `allow_from`         | array  | No       | Sender allowlist. Empty means everyone is allowed (the agent still only responds to accepted contacts).        |

> Allowlist entries use the canonical `simplex:<contactId>` form (the numeric
> SimpleX contact id), e.g. `"allow_from": ["simplex:3"]`. The contact id is
> logged when a message is received.

## Connecting a SimpleX app to the bot (step by step)

Once the `simplex-chat` CLI is running and PicoClaw is connected to it, you talk
to your bot like any other SimpleX contact: you add the **bot's contact address**
to your phone/desktop app, and the bot auto-accepts the request.

### 1. Get the bot's contact address

On its first start PicoClaw automatically creates a contact address for the bot
(if one doesn't exist yet) and **logs it**. The log line looks like:

```
INFO  simplex  Bot contact address (share this to let users connect)  address=https://simplex.chat/contact#/?v=2-7&smp=...
```

That `address=...` value (a `https://simplex.chat/contact#...` link) is what you
connect to.

> [!IMPORTANT]
> This line is logged at **info** level, but PicoClaw's default log level is
> `warn`, so you won't see it out of the box. For the first run, start the
> gateway with `info` logging:
> ```bash
> PICOCLAW_LOG_LEVEL=info picoclaw gateway
> ```
> or set `gateway.log_level` to `info` in your config. You only need this once -
> the address is stable, so note it down and you can switch back to `warn`.

Alternatively, get the address straight from the `simplex-chat` CLI: in an
interactive `simplex-chat` session run `/address` (creates and prints it) or
`/show_address` (prints the existing one). The CLI also renders it as a QR code
you can scan directly.

### 2. Add the bot in your SimpleX app (Android / iOS / desktop)

1. Open the SimpleX app and tap the **new chat / compose** button (the pencil
   or **+** icon).
2. Choose **"Connect via link / QR code"** (wording varies slightly per
   platform).
3. Either:
   - **Paste link:** switch to the *paste link* tab and paste the
     `https://simplex.chat/contact#...` address from step 1, then tap
     **Connect**; or
   - **Scan QR:** point the camera at the QR code shown by the `simplex-chat`
     CLI (`/address`).
4. Confirm the connection request. The bot (with `auto_accept: true`) accepts
   automatically - within a few seconds the contact turns into an active chat.

### 3. Say hi

Send a message to the new contact. PicoClaw forwards it to the agent and the
agent's reply comes back in the same chat. Send a photo or file and (with
`files_folder` configured) the agent receives it too.

> The same address is reusable: share it with multiple people and they can all
> connect to the same bot. Use `allow_from` (e.g. `["simplex:3"]`) if you want
> to restrict who the agent actually responds to.

## How it works

- On startup PicoClaw subscribes to chat events, ensures the bot has a contact
  address (creating one if needed), and logs that address so you can share it.
  Users connect by adding the bot's SimpleX address; with `auto_accept` on, the
  request is accepted automatically.
- Incoming direct messages are forwarded to the agent; the agent's replies are
  sent back to the same contact.
- **Inbound files:** set `files_folder` to the CLI's files directory. When a
  contact sends an image or file, PicoClaw triggers the download and, once it
  completes, passes the local file to the agent (vision pipeline) together with
  any caption.
- **Outbound files:** when the agent produces an image or file, it is sent to
  the contact as a SimpleX attachment.
- **Formatting:** SimpleX clients don't render HTML or full Markdown, only a few
  inline markers (`*bold*`, `_italic_`, `~strike~`, `` `code` ``) plus
  auto-linked URLs. PicoClaw automatically converts the agent's Markdown to this
  syntax: headings become bold, links become `label (url)`, lists become
  `•`/numbered lines, and tables become pipe-separated rows.

## Notes

- The `simplex-chat` CLI must stay running. If it restarts, PicoClaw reconnects
  automatically (when `reconnect_interval > 0`).
- Inbound attachments require the CLI and PicoClaw to share a filesystem so the
  downloaded file path is readable by PicoClaw.
