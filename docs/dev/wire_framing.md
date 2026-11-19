# Wire framing of the server-to-client stream

The server-to-client byte stream of a DTail session (over SSH, or in-process
in serverless mode) is a sequence of frames. Each frame ends with
`protocol.MessageDelimiter`, the single byte `0xAC`. A frame body is one of:

* `REMOTE|host|percent|line number|source id|content` — a log line
  (`protocol.Framing.EncodeLine`),
* `SERVER|host|message` and `AGGREGATE|host|data` — server messages and
  MapReduce aggregates (`protocol.Framing.EncodeMessage`),
* an untagged payload: a plain-mode log line, a hidden control message
  starting with `.` (e.g. `.syn capabilities ...`, `.syn close connection`),
  or, in plain mode, an `AUTHKEY OK` / `AUTHKEY ERR ...` reply. Sessions that
  negotiated control frames mark the control messages (see Control frames).

The client-to-server direction is unchanged: `protocol 4.1 base64 <command>;`.

The message of a `SERVER` frame that carries a diagnostic (a missing file,
the glob cap, a protocol version mismatch, a long line split, ...) is the
diagnostic as the server's logger formats it, e.g.
`WARN|1016-141259|user@host|Unable to read file(s), check server logs`. It is
worded with `logging.WarnMessage` / `logging.ErrorMessage`, which log it at
its level but return its text whatever the server's `--logLevel`, so a
dserver at `--logLevel error` or `none` still tells the client. A server
message without text (`""` or `"\n"`) is never framed, in any output mode.

## The problem with 0xAC

`0xAC` is `'¬'` truncated to a byte. It is also a UTF-8 continuation byte and
occurs in every character whose code point is `0x2C` modulo 64: `¬` (C2 AC),
`€` (E2 82 AC), `Ь` (D0 AC), `Ĭ`, `Ŭ` and about 1/64 of all CJK characters
(e.g. `丬`, E4 B8 AC). With the legacy raw framing a client cut a log line,
group key or aggregate value containing such a character at that byte, lost
the byte, and printed two broken lines (or failed to parse an aggregate).

## Escaped framing

`protocol.EscapedFraming` escapes two bytes in every frame body, header
fields included:

| body byte          | on the wire |
|--------------------|-------------|
| `0xAC` (delimiter) | `FF 01`     |
| `0xFF` (escape)    | `FF 02`     |

All other bytes are written unchanged. `0xFF` never occurs in valid UTF-8,
so a UTF-8 frame without `0xAC` (in particular every ASCII frame) is
byte-identical in both framings, and the escaped stream contains `0xAC` only
as the delimiter. Decoding (`Framing.DecodeFrame`) returns frames without
`0xFF` as they are, without copying; malformed sequences are kept verbatim.

Escaping and decoding take time linear in the body length whatever share of
the bytes must be escaped. The escaper (`escapeScanner`) keeps the positions
of the next `0xAC` and the next `0xFF` apart and only searches again for the
kind it just escaped, so every byte is scanned at most once per kind. Before
it escapes a long body, `writeEscaped` counts the bytes to escape (two
vectorised `bytes.Count` passes), grows the frame buffer once to the exact
escaped length and writes the escaped body into it, so a dense 1 MiB body
costs one allocation of its escaped size (at most 2 MiB); a body without
either byte costs the two counts and one copy. (An earlier version searched
the rest of the body for both bytes at every escape, which took about 50 s for
one 1 MiB line of `0xFF` and stalled the session; the next one grew the buffer
by the raw length only and reallocated while appending, 5 to 7 allocations of
6 to 11 MB in all for a dense 1 MiB body.)

## Control frames

Hidden messages and `AUTHKEY` replies are control messages: the client
consumes them instead of printing them. With `--plain` they used to be
untagged frames just like the log lines, so the client could only tell them by
their content: every log line starting with `.` vanished from `--plain`
server-mode output, so did lines like `AUTHKEY OK` (also with surrounding
blanks or as `SERVER|x|AUTHKEY OK`), and a log line could forge a control
message: `.syn close connection` made the client acknowledge the close and
count the session as complete, `.syn command failed x` added a failed command
(and so, with the dcat/dgrep exit statuses, exit status 2), `.syn
capabilities ...` added capabilities. Serverless mode printed all of them.

`protocol.EscapedControlFraming` removes the ambiguity. It escapes every frame
body exactly like `EscapedFraming` and additionally starts the body of every
control frame with the marker `FF 03` (`Framing.EncodeControl`):

| frame           | body on the wire                      |
|-----------------|---------------------------------------|
| control frame   | `FF 03` + escaped message             |
| any other frame | escaped body, as under EscapedFraming |

Escaping turns every `0xFF` of a body into `FF 02`, so no escaped body starts
with `FF 03`: payload cannot forge the marker, and a frame without it is
payload whatever it contains. The client cuts the marker off before decoding
(`Framing.CutControl`; a decoded body may well start with the bytes
`FF 03`). In such a session the client handles marked frames as hidden
messages or `AUTHKEY` replies (and ignores other marked frames of newer
servers) and prints every unmarked frame (`frameKind` in
`internal/clients/handlers/basehandler.go`); MapReduce sessions never take a
marked frame for aggregate data. The server marks every hidden message and
every `AUTHKEY` reply, in plain and REMOTE line format alike (the latter
replaces the `SERVER|host|AUTHKEY ...` frame of other sessions). Line frames,
server messages and aggregates are byte-identical to `EscapedFraming`, so the
per-line hot path does not change.

## Escaped frames and the output buffer cap

Escaping turns each `0xAC` and `0xFF` of a body into two bytes, so the frame of
a maximum-length line made of them is almost twice its raw size, and so is
the line writer batch holding it. `OutputBufferMaxBytes` must only be at least
`MaxLineLength + 128 KiB` (sized for raw frames), so a session that switches to
escaped framing raises its own output buffer cap to at least
`2 * MaxLineLength + 128 KiB` (`escapedOutputBufferMaxBytes`; a larger
configured cap is kept). Every configuration that is valid today keeps
working, and the memory a session may hold for a slow client stays bounded,
at most one more maximum-length line than configured. Raw sessions keep the
configured cap.

Should a batch still not fit (it cannot with the frames dserver writes, as
the 64 KiB of the reserve cover the escaped line header), the read reports it
instead of dropping it silently: the client gets the warning `Dropped output
too large for the session output buffer` and the failed-command reason
`output: too large for the session output buffer`
(`protocol.CommandFailureOutputTooLarge`, no file read failure). That holds on
every read path, the private reader of a shared follow session after its
eviction included: the hub passes the error to `readhub.Session.ReportFailure`,
and the read command reports it like its own read loop does.

## Negotiation and mixed versions

The framing is chosen per session, so every client and server version keeps
working with every other one:

1. The server queues its capabilities frame (`.syn capabilities ...`) when the
   session handler is built. It is the first frame of the session and always
   raw. dserver and the in-process serverless server list
   `escaped-framing-v1` (`protocol.CapabilityEscapedFramingV1`) and
   `control-frames-v1` (`protocol.CapabilityControlFramesV1`).
2. The client handlers for dcat, dgrep, dtail and dmap (`NewClientHandler`,
   `NewMaprHandler`) send `.ack framing escaped-v1`
   (`protocol.EscapedFramingCommand`) as their very first command frame and
   `.ack framing control-v1` (`protocol.ControlFramesCommand`) as the second,
   both in one write, before `AUTHKEY` and `.ack input begin`, whichever
   connector drives them. They do not wait for the capabilities, so the
   requests cost no round trip.
3. A server that advertised the capability switches the session to escaped
   framing when it handles the request (`ServerHandler.handleAckControl`), but
   only when the request is the session's first command frame; a later or
   repeated request is ignored and logged (`Ignoring escaped framing request
   that is not the session's first command`), so the framing of output that
   earlier commands produce never changes. The switch waits until the
   capabilities frame has been encoded (`sessionFramer.enableEscapedFraming`;
   in practice it never waits) and runs on the command input goroutine, so
   every frame of a later command is escaped, including line frames that a
   read command pre-encodes (`NewNetworkWriter`, whose framing argument is
   required).
4. A server that advertised `control-frames-v1` switches an escaped session
   on to `EscapedControlFraming` when it handles the control frames request
   as the session's second command
   (`ServerHandler.handleControlFramesRequest`,
   `sessionFramer.enableControlFrames`); a request that is not the second
   command, or reaches a session that is not escaped, is ignored and logged.
   Either request is honoured only as the exact command frame clients send:
   one with a `timeout N` prefix is ignored and logged too
   (`commandDispatcher.dispatchCommand`).
   The server encodes no frame after the capabilities frame until the
   session framing has settled (`sessionFramer.awaitFramingSettled`), so
   every frame is encoded in the framing the client decided on: a control
   message the server sends on its own before the client's commands arrive,
   such as the close handshake of a dserver shutting down in the session's
   first moment, waits until then and reaches a client of a marking session
   marked. The framing settles before the server handles a command that
   can no longer change it (`sessionFramer.settleFramingBeforeCommand`): a
   first command that is not the escaped framing request (an older
   client's first command), a second one that is not the control frames
   request (an escaped-only client's second one), and a command frame that
   does not decode. It settles before, not after, such a command because
   handling it may wait for output: a `SESSION START` acks its spec only
   once the server message channel has room, which its reads can fill
   (e.g. with one `Unable to read file(s)` message per missing file), and
   held back output would never make room. An unknown command or failed
   map query that is the session's only command does not block the command
   input goroutine: it starts the session's shutdown, whose flush waits for
   the output, on a goroutine of its own (`ServerHandler.finishUserCommand`),
   so the client's close acknowledgement is handled at once instead of
   after the 5 s acknowledgement timeout. A command frame the server
   rejects before any handler ran (a frame that does not decode, an
   invalid `timeout N` prefix or options that do not parse) admits no
   command, so none finishes; when it leaves the session without work (no
   other command, pending read, open input batch or `SESSION`), it starts
   the same shutdown the same way (`ServerHandler.frameRejected`), right
   after its error and failure report, instead of leaving the session open
   until `IdleSessionTimeoutS`; a health session
   (`HealthHandler.frameRejected`) does so for any rejected frame. A frame
   of another protocol version (or none) gets its mismatch error, once per
   session. A frame of a legacy client then leaves the session open, as
   before: it ends when the client exits or at `IdleSessionTimeoutS`.
   Legacy clients are those of protocol major version <= 3, of an
   unparsable version, or sending no `protocol` prefix at all (v1.x clients
   frame commands as `%s;`; their error gets no trailing newline, as it
   never did); `protocolCodec.handleProtocolVersion` reports this as
   `legacy`. Such clients do not check the close handshake, and those that
   retry (v1.x-v3 `dtail`, v3 `dmap` in tail mode without outfile)
   reconnect 2 s after a session ended, so ending it would make them
   reconnect in a loop, each time with a full SSH handshake and server log
   lines. A frame of any other version, e.g. of a future 4.x+ client,
   then, under the same conditions, ends the session without the
   close handshake
   (`ServerHandler.versionRejected`, `HealthHandler.versionRejected`,
   `baseHandler.closeWithoutHandshake`): the server flushes the error and
   closes the session, with no `.syn close connection` and no wait for an
   acknowledgement. Such a client can not be relied on to handle the
   handshake: a current client of another version sends its framing
   requests in that version too, so the server rejects them and the
   session stays raw, while the client already chose marked framing from
   the capabilities frame and would print the unmarked close request as
   payload and never acknowledge it. Such clients thus see the error and
   the session ends at once. Clients that check for the close handshake
   (this fork since ef3) treat it as cut short: `dcat`/`dgrep` exit 1, a
   scheduled job counts a transport failure, `dtailhealth` reports
   CRITICAL. Further frames of another version get no error message, and
   none at all once the session is closing
   (`commandDispatcher.rejectProtocolVersion`).
   Settling before the command has a
   cost: its replies may follow a frame queued before it, e.g. the close
   handshake of a dserver shutting down in the session's first moment, as
   they always did before control frames existed; clients acknowledge the
   close either way. The framing also settles after an escaped framing
   request when the server does not advertise `control-frames-v1` or the
   request switched nothing, after the second command in any case, and
   when the session ends (e.g. its input ended). Clients send the requests
   in one write, together with or right before their first commands, so
   the frames are held back for no noticeable time; a held back read does
   not count as an active one, so a flush with nothing queued does not
   wait for it either.
5. The client decides the framing from the session's first frame only: if
   that frame is the capabilities frame and lists `escaped-framing-v1` (and
   the client sent the requests), it decodes every later frame as escaped,
   and as `EscapedControlFraming` if the frame also lists
   `control-frames-v1`; otherwise the session stays raw for good. A later
   frame that reads like a capabilities frame, e.g. a log line of a raw
   `--plain` session from an older server, does not switch it.

| client \ server                  | older dserver          | dserver with escaped framing only | current dserver |
|----------------------------------|------------------------|-----------------------------------|-----------------|
| older client (no request)        | raw                    | raw                               | raw             |
| client with escaped framing only | unsafe (may close early) | escaped                           | escaped         |
| current client (both requests)   | unsupported (status 1) | escaped (2nd `.ack` ignored)      | escaped-control |

Only the escaped-control combination prints `--plain` log lines that read like
control messages and never acts upon them; every other combination keeps the
content-based routing described under Control frames, with its loss of such
lines, as before (checked with dcat of this version against a dserver with
escaped framing only, and the other way round). Output without `--plain` is
unambiguous in every combination, since log lines are `REMOTE|...` frames
there.

Current clients require `escaped-framing-v1` in the first server frame.
Without it they report `unsupported DTail server: missing capability
escaped-framing-v1; please update DTail server` and set status 1, even if the
server sends a normal close handshake. Some older protocol-4.1 servers count
unknown `.ack` commands as completed work and close before queued reads run;
both framing requests and input-batch markers can trigger that. Treating
these peers as unsupported prevents successful empty results. This is an
explicit compatibility boundary despite the unchanged `ProtocolCompat` 4.1.
Servers with escaped framing only remain supported, with content-based control
routing. Older clients can still read current servers using raw framing.

Released v4.3.x dservers also predate escaped framing and are unsupported
by current clients. Version and protocol labels alone do not establish that
a peer supports the session controls.

The health check (`dtailhealth`) never requests escaped framing or control
frames: the server's health handler ends the session after its first command
and advertises no capabilities.

## Where the code lives

* `internal/protocol/framing.go` — `Framing`, escaping and decoding
* `internal/protocol/wire.go` — `Framing.EncodeLine`, `Framing.EncodeMessage`,
  and `LineEncoder`, the server's per-line hot path, which escapes the
  constant header fields once per line writer (with escaped framing a typical
  line frame costs fewer instructions than the raw `EncodeLine` did before,
  which also allocated its line number string)
* `internal/protocol/control_frames.go` — control frames (`EncodeControl`,
  `CutControl`, `IsControlMessage`)
* `internal/protocol/capabilities.go` — capability and request constants
* `internal/handlers/session_framer.go` — the server session's framing state
  and the framing of control messages (`readServerMessage`)
* `internal/handlers/serverhandler.go` — handling of the requests
* `internal/handlers/line_writer.go`, `protocol_formatter.go` — line frames
* `internal/clients/handlers/basehandler.go`, `maprhandler.go` — request and
  decoding on the client
* `internal/handlers/output_manager.go` — `raiseBufferMaxBytes`, the escaped
  session's output buffer cap
* `integrationtests/utf8_framing_test.go` — server mode versus serverless
  dcat (plain and REMOTE frames), dgrep (REMOTE frames) and dmap on `€`, `Ь`,
  `¬` and `丬`, a dtail follow of such lines, and 1 MiB lines of only `0xAC`,
  only `0xFF` and only `€` at the default `OutputBufferMaxBytes`
* `integrationtests/plain_control_frames_test.go` — `--plain` dcat and dgrep
  in server mode versus serverless on lines such as `.hidden`, `.syn close
  connection`, `.syn command failed x` and `AUTHKEY OK` (with exit status 0),
  and a `dtail --plain` follow that goes on after them
