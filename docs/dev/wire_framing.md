# Wire framing of the server-to-client stream

The server-to-client byte stream of a DTail session (over SSH, or in-process
in serverless mode) is a sequence of frames. Each frame ends with
`protocol.MessageDelimiter`, the single byte `0xAC`. A frame body is one of:

* `REMOTE|host|percent|line number|source id|content` — a log line
  (`protocol.Framing.EncodeLine`),
* `SERVER|host|message` and `AGGREGATE|host|data` — server messages and
  MapReduce aggregates (`protocol.Framing.EncodeMessage`),
* an untagged payload: a plain-mode log line,
* a control frame, marked as such: a hidden control message starting with `.`
  (e.g. `.syn capabilities ...`, `.syn close connection`, the probe answer
  `.syn probe <id> <base64>` described in `probe.md`) or an
  `AUTHKEY OK` / `AUTHKEY ERR ...` reply (see Control frames).

Since protocol 5 every frame of every session, from the first one on, is
written in `protocol.EscapedControlFraming` (escaped bodies, marked control
frames); nothing is negotiated (see "Protocol 5: one framing"). The
client-to-server direction is unchanged apart from the version:
`protocol 5 base64 <command>;`.

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
(e.g. `丬`, E4 B8 AC). With the raw framing of protocol 4.1 and earlier a
client cut a log line, group key or aggregate value containing such a
character at that byte, lost the byte, and printed two broken lines (or failed
to parse an aggregate).

## Escaped framing

`protocol.EscapedControlFraming` escapes two bytes in every frame body,
header fields included:

| body byte          | on the wire |
|--------------------|-------------|
| `0xAC` (delimiter) | `FF 01`     |
| `0xFF` (escape)    | `FF 02`     |

All other bytes are written unchanged. `0xFF` never occurs in valid UTF-8,
so a UTF-8 frame without `0xAC` (in particular every ASCII frame) is
written byte for byte, and the escaped stream contains `0xAC` only as the
delimiter. Decoding (`Framing.DecodeFrame`) returns frames without
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
(before control frames, and in every session before protocol 5 that did not
negotiate them) untagged frames just like the log lines, so the client could
only tell them by their content: every log line starting with `.` vanished from `--plain`
server-mode output, so did lines like `AUTHKEY OK` (also with surrounding
blanks or as `SERVER|x|AUTHKEY OK`), and a log line could forge a control
message: `.syn close connection` made the client acknowledge the close and
count the session as complete, `.syn command failed x` added a failed command
(and so, with the dcat/dgrep exit statuses, exit status 2), `.syn
capabilities ...` added capabilities. Serverless mode printed all of them.

`protocol.EscapedControlFraming` removes the ambiguity. Besides escaping every
frame body, it starts the body of every control frame with the marker `FF 03`
(`Framing.EncodeControl`):

| frame           | body on the wire          |
|-----------------|---------------------------|
| control frame   | `FF 03` + escaped message |
| any other frame | escaped body              |

Escaping turns every `0xFF` of a body into `FF 02`, so no escaped body starts
with `FF 03`: payload cannot forge the marker, and a frame without it is
payload whatever it contains. The client cuts the marker off before decoding
(`Framing.CutControl`; a decoded body may well start with the bytes
`FF 03`). The client handles marked frames as hidden messages or `AUTHKEY`
replies (and ignores other marked frames of newer servers) and prints every
unmarked frame (`frameKind` in `internal/clients/handlers/basehandler.go`);
MapReduce sessions never take a marked frame for aggregate data. The server
marks every hidden message and every `AUTHKEY` reply, in plain and REMOTE
line format alike. Line frames, server messages and aggregates carry no
marker, so the per-line hot path only escapes.

## Escaped frames and the output buffer cap

Escaping turns each `0xAC` and `0xFF` of a body into two bytes, so the frame of
a maximum-length line made of them is almost twice its raw size, and so is
the line writer batch holding it. `OutputBufferMaxBytes` must only be at least
`MaxLineLength + 128 KiB` (sized for unescaped frames), so every client
session (`ServerHandler`; a health session sends only a few short frames and
keeps the default) raises its own output buffer cap to at least `2 * MaxLineLength + 128 KiB`
(`escapedOutputBufferMaxBytes`, `sessionOutputBufferMaxBytes`; a larger
configured cap is kept). Every configuration that is valid keeps working, and
the memory a session may hold for a slow client stays bounded, at most one
more maximum-length line than configured.

Should a batch still not fit (it cannot with the frames dserver writes, as
the 64 KiB of the reserve cover the escaped line header), the read reports it
instead of dropping it silently: the client gets the warning `Dropped output
too large for the session output buffer` and the failed-command reason
`output: too large for the session output buffer`
(`protocol.CommandFailureOutputTooLarge`, no file read failure). That holds on
every read path, the private reader of a shared follow session after its
eviction included: the hub passes the error to `readhub.Session.ReportFailure`,
and the read command reports it like its own read loop does.

## Protocol 5: one framing

Protocol 4.1 negotiated the framing per session (`.ack framing escaped-v1` and
`.ack framing control-v1` as the client's first two commands, raw or
escaped-only framing otherwise, and a server holding back its frames until
the framing settled). Protocol 5 (`protocol.ProtocolCompat`) drops all of
that, as a peer of another protocol version is rejected anyway:

1. The server encodes its capabilities frame (`.syn capabilities ...`) when
   the session handler is built and holds it as the session framer's first
   pending output (`sessionFramer.queueFirstFrame`), which the output reads
   before any queue: payload, MapReduce messages and other server messages
   travel through different queues and could otherwise overtake it. It is
   the first frame of a client session and,
   like every frame after it, written in `EscapedControlFraming`: a marked
   control frame. dserver and the in-process serverless server send the same
   frames (`sessionFraming` in `internal/handlers/session_framer.go`); the
   server never waits for a client request before it sends a frame.
2. The client handlers for dcat, dgrep, dtail and dmap (`NewClientHandler`,
   `NewMaprHandler`) decode every frame in `EscapedControlFraming` and send
   no framing request; their first command frame is their first command
   (e.g. `AUTHKEY` or `.ack input begin`). They check the session's first
   frame (`baseHandler.checkFirstFrame`): unless it is a marked capabilities
   frame (which may list no capability; only runtime properties such as
   `journal-v1` are advertised), the server is of an older protocol
   version, and the client reports `unsupported DTail server: not a protocol
   5 server (no capabilities frame first); please update DTail server` and
   sets status 1, even
   if the server then sends a normal close handshake. Some protocol-4.1
   servers count unknown `.ack` commands as completed work and close before
   queued reads run; treating them as unsupported prevents successful empty
   results. Such a server's unmarked capabilities frame is not printed; its
   other frames (e.g. its version mismatch error) are, as unmarked payload.
3. The health check (`dtailhealth`, client and server `HealthHandler`) uses
   the same framing; the server's health handler sends no capabilities frame,
   so the health client does not check the first frame.
4. The output buffer cap of every client session is always the escaped one
   (see above); health sessions keep the default.

The server rejects every command frame of another protocol version
(`commandDispatcher.rejectProtocolVersion`) and sends its mismatch error
once per session; further frames of another version get no error message,
and none at all once the session is closing.
`protocolCodec.handleProtocolVersion` tells the session handler which kind
of client sent the frame (`mismatchedClient`):

* Older clients keep their session in dserver (`ServerHandler.versionRejected`):
  it ends when the client exits or at `IdleSessionTimeoutS`. These are
  clients of protocol major version <= 4, of an unparsable version, or
  sending no `protocol` prefix at all (v1.x clients frame commands as `%s;`;
  their error gets no trailing newline, as it never did; protocol 2-3
  clients get one). Clients that retry reconnect a few seconds after a
  session ended (v1.x-v3 `dtail` and v3 `dmap` in tail mode without outfile
  after 2 s), so ending it would make them reconnect in a loop, each time
  with a full SSH handshake and server log lines. Observed with 4.1 builds
  of this fork from before protocol 5 (3ea4784) against a protocol 5
  dserver that ended the session: their `dtail` reconnected about every
  3 s until stopped, their `dcat` and `dmap` exited 1. With the session
  kept open, every such client (released ones included) waits after the
  mismatch error until `IdleSessionTimeoutS` or until the user stops it.
* The health handler keeps only clients of protocol 3 and lower (and of
  unparsable or missing versions) open (`HealthHandler.versionRejected`):
  `dtailhealth` does not reconnect, so a 4.x `dtailhealth` gets its session
  ended at once and its check fails.
* A frame of any other version (a future one, or another 5.x minor
  version) ends the session without the close handshake
  (`baseHandler.closeWithoutHandshake`): the server flushes the error and
  closes the session, with no `.syn close connection` and no wait for an
  acknowledgement, as such a client may frame differently and can not be
  relied on to acknowledge the close. Clients that check for the close
  handshake treat it as cut short: `dcat`/`dgrep` exit 1, a scheduled job
  counts a transport failure, `dtailhealth` reports CRITICAL.

An older client does not decode protocol 5 framing: what holds for every
such client, released 4.x and <= 3 ones included, is that it gets the
mismatch error (`... does not match client protocol version '...', please
update DTail client`), possibly after unreadable output, as the capabilities
frame is sent before the server knows the client's version. This can not be
avoided without delaying every session's first frame; it is accepted, as
upgrading the client is the fix either way. The pre-protocol-5 4.1 builds of
this fork (e.g. 3ea4784) print the marked capabilities frame as payload (the
bytes `FF 03` followed by `.syn capabilities ...`) and report `unsupported
DTail server: missing capability escaped-framing-v1; please update DTail
server` before the mismatch error; released 4.x clients (e.g. v4.3.4) have
no such check.

Handling a command may wait for output: a `SESSION START` acks its spec only
once the acknowledgement queue has room; the session reader drains it
meanwhile. The session reader sends payload before server messages, but a
`SESSION START`/`UPDATE` acknowledgement, ok or error, goes ahead of queued
payload as soon as that splits no frame (`tryReadSessionAckFirst`,
`outputManager.atRecordBoundary`), and an ok acknowledgement ahead of every
output of the workload it acknowledges (`sessionAckGate` in
`internal/handlers/session_ack_gate.go`): before the workload's commands are
dispatched its generation is held, its payload stays queued
(`outputManager.tryRead` stops at its first entry) and its server and
MapReduce messages (e.g. one `Unable to read file(s)` message per missing
file, failure reports) are deferred in order, until the reader took the
acknowledgement from its own queue (`sessionAcks`), which ends the hold. So
no acknowledgement waits behind the session output buffer on a slow link
(the client waits 2 s for it), also not the error acknowledgement of a
rejected update while the running workload fills the buffer, and an ok one
means the workload is active. A frame already begun is finished first; the
rest of a replaced generation is stale and dropped. A workload that fails
to start releases its hold on the input goroutine and gets an error
acknowledgement. The deferred messages are transient, about three per read
command of the spec. An unknown command or failed map query that is the
session's only command does not block the command input goroutine: it
starts the session's shutdown, whose flush waits for the output, on a
goroutine of its own (`ServerHandler.finishUserCommand`), so the client's
close acknowledgement is handled at once instead of after the 5 s
acknowledgement timeout. A command frame the server rejects before any
handler ran (a frame that does not decode, an invalid `timeout N` prefix or
options that do not parse) admits no command, so none finishes; when it
leaves the session without work (no other command, pending read, open input
batch or `SESSION`), it starts the same shutdown the same way
(`ServerHandler.frameRejected`), right after its error and failure report,
instead of leaving the session open until `IdleSessionTimeoutS`; a health
session (`HealthHandler.frameRejected`) does so for any rejected frame.

## Where the code lives

* `internal/protocol/framing.go` — `Framing`, escaping and decoding
* `internal/protocol/wire.go` — `Framing.EncodeLine`, `Framing.EncodeMessage`,
  and `LineEncoder`, the server's per-line hot path, which escapes the
  constant header fields once per line writer (a typical escaped line frame
  costs fewer instructions than the raw `EncodeLine` of protocol 4.1 did,
  which also allocated its line number string)
* `internal/protocol/control_frames.go` — control frames (`EncodeControl`,
  `CutControl`, `IsControlMessage`)
* `internal/protocol/capabilities.go` — capability constants
* `internal/handlers/session_framer.go` — `sessionFraming` and the framing of
  control messages (`readServerMessage`)
* `internal/handlers/line_writer.go`, `protocol_formatter.go` — line frames
  (`protocol.TerminalFraming` lends the frame layout to serverless terminal
  output, which is never sent as frames)
* `internal/clients/handlers/basehandler.go`, `maprhandler.go`,
  `healthhandler.go` — decoding and the first frame check on the client
* `internal/handlers/serverhandler.go` — `sessionOutputBufferMaxBytes`, the
  session's output buffer cap
* `integrationtests/utf8_framing_test.go` — server mode versus serverless
  dcat (plain and REMOTE frames), dgrep (REMOTE frames) and dmap on `€`, `Ь`,
  `¬` and `丬`, a dtail follow of such lines, and 1 MiB lines of only `0xAC`,
  only `0xFF` and only `€` at the default `OutputBufferMaxBytes`
* `integrationtests/plain_control_frames_test.go` — `--plain` dcat and dgrep
  in server mode versus serverless on lines such as `.hidden`, `.syn close
  connection`, `.syn command failed x` and `AUTHKEY OK` (with exit status 0),
  and a `dtail --plain` follow that goes on after them
