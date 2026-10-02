# 12 — PostgreSQL Wire Protocol (`internal/pgwire`, `internal/server`)

Status: **Designed for Phase 5 (Steps 5.1–5.4); approved. Implemented so far: 5.1, 5.2.** Step 5.1 (startup and authentication) is designed in full here. Steps 5.2–5.4 are outlined in section 2.6 so that 5.1's choices fit them, and are detailed in this document when their step starts.

## 1. Problem

NoVacDB must speak PostgreSQL's frontend/backend protocol, version 3.0, so that `psql`, `pgbench` and standard drivers connect without special support. Step 5.1 covers everything from TCP connect to the first `ReadyForQuery`: TLS and GSS encryption requests (declined), the startup message and its parameters, authentication (trust), the session parameters the server reports, and the cancel key.

It must also be robust: a client is untrusted input. A malformed, oversized, slow or abandoned connection must cost a bounded amount of memory and time, end with a clear error, and never crash the server or affect other connections.

## 2. Design

### 2.1 Layers

- **`internal/pgwire`**: the protocol's messages, with no networking or database knowledge. A `Reader` reads framed messages from an `io.Reader` with a size limit; a `Writer` builds backend messages into a buffer and flushes them. Pure functions decode the startup packet and encode each backend message. Everything here is testable on byte slices and fuzzable.
- **`internal/server`**: `Server` accepts TCP connections, runs one goroutine per connection, performs the startup handshake, and (from 5.2) hands queries to `executor.DB`. It owns connection IDs and cancel keys, timeouts, and shutdown.
- **`cmd/novacdb`**: opens the database in `--data-dir` on the real file system (`vfs.OSFS`), starts the server on `--listen` (default `localhost`) and `--port` (default 5433), and stops on SIGINT/SIGTERM (graceful shutdown is completed in 5.4).

### 2.2 Framing

All integers big-endian (the protocol's byte order; the storage engine's little-endian rule is about NoVacDB's own files).

- **Startup-phase packets** have no type byte: `Int32 length` (including itself) then `Int32 code`, then a body. Length must be at least 8 and at most **10,000 bytes** (PostgreSQL's `MAX_STARTUP_PACKET_LENGTH`).
- **Regular messages**: `Byte1 type`, `Int32 length` (including itself, not the type), body. Length must be at least 4 and at most **`MaxMessageSize` = 16 MiB**. PostgreSQL allows up to 1 GiB, but NoVacDB's queries are at most 1 MiB (09-sql-frontend.md) and rows at most a page, so 16 MiB leaves room for parameters (5.3) while bounding what a client can make the server allocate. A larger length is a protocol violation.
- A length outside its bounds, or a message whose body does not parse exactly (missing terminators, trailing bytes), is **`08P01` protocol_violation**: the server sends a `FATAL` `ErrorResponse` and closes the connection, as PostgreSQL does. It never reads the declared body of an oversized message.

### 2.3 Startup (Step 5.1)

```mermaid
sequenceDiagram
    participant C as client
    participant S as NoVacDB
    C->>S: SSLRequest (80877103)
    S->>C: 'N'
    C->>S: StartupMessage (3.0, user, database, ...)
    S->>C: AuthenticationOk
    S->>C: ParameterStatus × 13
    S->>C: BackendKeyData (process ID, secret)
    S->>C: ReadyForQuery ('I')
```

The first packet's code decides what it is:

| code | packet | server action |
|---|---|---|
| 80877103 | `SSLRequest` | Reply `N` (no TLS) and read the next startup packet. |
| 80877104 | `GSSENCRequest` | Reply `N` and read the next startup packet. |
| 80877102 | `CancelRequest` | Step 5.4. In 5.1, close the connection without a reply (a cancel request never gets one). |
| major 3 (`0x0003xxxx`) | `StartupMessage` | Continue below. |
| anything else | | `FATAL 0A000` "unsupported frontend protocol *M.m*: server supports 3.0 to 3.0", close. |

At most two encryption requests (one of each kind) are accepted before the startup message; a third, or one after the other was declined twice, is `08P01`. A client that sends data after its `SSLRequest` but before reading the `N` is a protocol violation in PostgreSQL (it may be a TLS downgrade attack); NoVacDB does the same: if bytes are already buffered after an encryption request, `08P01`.

**StartupMessage.** After the code: name/value pairs of NUL-terminated strings, ended by an empty name. Duplicates: the last one wins, as in PostgreSQL.

- **Protocol minor version.** NoVacDB speaks 3.0. A client asking for 3.*n* with *n* > 0 (`psql` 18 asks for 3.2) gets a `NegotiateProtocolVersion` message (`v`: newest minor supported, 0, and the list of `_pq_.`-prefixed options it did not recognise), and the session continues in 3.0. Any `_pq_.` option is likewise listed and ignored, even with 3.0.
- **`user`** is required: missing or empty is `FATAL 28000` "no PostgreSQL user name specified in startup packet".
- **`database`** defaults to the user name. NoVacDB has one database per data directory, so **any database name is accepted** (decision: rejecting names would make plain `psql -h localhost -p 5433` fail, since `psql` asks for a database named after the OS user). The name is kept for the session and logged.
- **`application_name`**: kept, reported back, and logged. At most 63 bytes (PostgreSQL truncates; so does NoVacDB).
- **`client_encoding`**: `UTF8` (any case, with or without the hyphen, or `UNICODE`) is accepted, and so is `SQL_ASCII`, which PostgreSQL also accepts with a UTF-8 server: it means "no conversion", and libpq sends it from terminals in the C locale (found while implementing: refusing it would stop `psql` from connecting there). Anything else is `FATAL 22023` `invalid value for parameter "client_encoding": "LATIN1"`: NoVacDB stores and sends only UTF-8, and has no conversions.
- **`DateStyle`**: accepted if it is ISO output (`ISO`, `ISO, MDY`, `ISO, DMY`, `ISO, YMD`, any case and spacing); reported as `ISO, MDY`. Otherwise `FATAL 22023`.
- **`TimeZone`**: accepted if it names UTC (`UTC`, `Etc/UTC`, `GMT`, `Etc/GMT`, `Z`, `+00`, `0`); anything else is `FATAL 0A000` "time zone "Europe/Berlin" is not supported yet: the session time zone is always UTC" (10-executor.md section 8). Drivers that send the JVM's time zone (pgjdbc) need `-Duser.timezone=UTC` until time zones arrive.
- **`extra_float_digits`**: accepted (any integer −15..3) and ignored: NoVacDB always prints the shortest exact form, which is what PostgreSQL prints for values ≥ 1, the default since PostgreSQL 12.
- **`search_path`**, **`statement_timeout`** and **`lock_timeout`**: accepted and ignored with a log line (no schemas yet; timeouts arrive with transactions). Ignoring them silently changes nothing a client can observe today.
- **`options`**: the command-line form `-c name=value` and `--name=value`, split on unescaped spaces with backslash escapes as libpq does; each setting goes through the same rules.
- **`replication`**: `FATAL 0A000` "replication is not supported".
- **Anything else**: `FATAL 42704` `unrecognized configuration parameter "foo"`, as PostgreSQL.

**Authentication: trust.** After the startup message the server sends `AuthenticationOk` (`R`, 0). There is no password check. Because of that, **the server listens on `localhost` by default**, as PostgreSQL does, and logs a warning at startup when `--listen` names a non-loopback address. Password authentication (SCRAM-SHA-256) is on the roadmap before any network deployment; it needs only the `R` subcodes 10–12 and the SASL exchange, which this framing already supports.

**ParameterStatus** (`S`), the 13 parameters PostgreSQL 16 reports, in its order:

| name | value |
|---|---|
| `application_name` | from the startup message, or empty |
| `client_encoding` | `UTF8` |
| `DateStyle` | `ISO, MDY` |
| `default_transaction_read_only` | `off` |
| `in_hot_standby` | `off` |
| `integer_datetimes` | `on` |
| `IntervalStyle` | `postgres` |
| `is_superuser` | `on` |
| `server_encoding` | `UTF8` |
| `server_version` | `16.0 (NoVacDB 0.5)` |
| `session_authorization` | the user |
| `standard_conforming_strings` | `on` |
| `TimeZone` | `UTC` |

`server_version` starts with 16.0 because drivers parse its leading number to decide which SQL they may send (libpq's `PQserverVersion`, pgx, pgjdbc), and NoVacDB's SQL follows PostgreSQL 16; the parenthesised part says what it really is. `psql` 16 therefore connects without its version-mismatch warning.

**BackendKeyData** (`K`): a 32-bit process ID and a 32-bit secret. NoVacDB has no processes, so the "process ID" is a connection number, starting at a random value and increasing (never 0); the secret comes from `crypto/rand`. The server keeps the pair while the connection is open, so 5.4's `CancelRequest` can find it. Under protocol 3.0 the key is exactly 4 bytes (3.2's longer keys come with 3.2).

**ReadyForQuery** (`Z`, `I`): the session is idle. Step 5.1 ends here.

### 2.4 After startup, in Step 5.1

(Replaced by section 2.8 in Step 5.2.) Only `Terminate` (`X`) is fully handled: the server closes the connection. A `Query` (`Q`) gets an `ERROR 0A000` "the simple query protocol is not implemented yet" followed by `ReadyForQuery`, so `psql` stays usable and reports it, until 5.2 replaces it. Any other message type is `FATAL 08P01` "invalid frontend message type *N*" and the connection closes, as PostgreSQL does for unknown types.

### 2.5 Errors and notices

`ErrorResponse` (`E`) and `NoticeResponse` (`N`) carry fields: `S` severity (`ERROR`, `FATAL`, `NOTICE`), `V` the same, unlocalised (PostgreSQL 9.6+), `C` SQLSTATE, `M` message, and when present `D` detail, `H` hint, `P` position (1-based characters, as `sqlerr.Error` already holds). A `FATAL` error ends the connection after it is sent. Errors of the executor are `*sqlerr.Error` already, so 5.2 maps them field by field.

### 2.6 Steps 5.2–5.4, in outline

- **5.2 Simple query.** Designed in 2.8.
- **5.3 Extended query.** `Parse`/`Bind`/`Describe`/`Execute`/`Sync`/`Close`/`Flush`, named and unnamed statements and portals, text-format parameters (`$1` binds as an untyped literal of the parameter's declared or inferred type), binary result formats for the six types. Errors skip to `Sync`.
- **5.4 Connections.** A connection limit (`--max-connections`, default 1000: goroutines are cheap; problem #12/#13 in WORKFLOW.md), `CancelRequest` (cancels the target connection's statement context; a cancelled write that has begun applying still finishes, 10-executor.md section 2.4), idle and startup timeouts, and graceful shutdown: stop accepting, let running statements finish up to a deadline, send `FATAL 57P01` "terminating connection due to administrator command", close the database (final checkpoint).

### 2.7 Implementation notes (Step 5.1)

- **`SQL_ASCII` client encoding** is accepted (2.3), a change from the reviewed design: libpq sends it from terminals in the C locale, and PostgreSQL accepts it.
- **Large message bodies grow as they arrive.** A body up to 64 KiB is allocated at once; a larger one is read into a buffer that grows with the bytes received, so announcing 16 MiB and stalling costs nothing.
- **Startup errors come before `AuthenticationOk`.** PostgreSQL checks startup settings after authentication; with trust authentication the difference is invisible to clients, and refusing early sends less.
- **The cancel request code is protocol "1234.5678".** A version check that looked only at the major number would treat it as protocol 1234; the startup loop recognises the three request codes first, so the tests' "unsupported protocol" cases use 1234.5677.
- **Verified with real clients:** `psql` 16 connects (with `sslmode=prefer`, after the declined `SSLRequest`), shows the 5.1 query error, and `\conninfo` reports the database and user; `pg_isready` reports "accepting connections"; a refused `TimeZone` setting is shown as the server's `FATAL`; the server stops cleanly on SIGTERM. An integration test runs `psql` whenever it is installed.

### 2.8 Simple query protocol (Step 5.2)

A `Query` message carries a string of zero or more statements. The server runs it with `executor.DB.Exec`, which runs the statements in order and stops at the first error, and answers:

```mermaid
sequenceDiagram
    participant C as client
    participant S as NoVacDB
    C->>S: Query "CREATE ...; INSERT ...; SELECT ..."
    S->>C: CommandComplete "CREATE TABLE"
    S->>C: CommandComplete "INSERT 0 2"
    S->>C: RowDescription, DataRow × n, CommandComplete "SELECT n"
    S->>C: ReadyForQuery 'I'
```

- **For each statement that ran:** its notices as `NoticeResponse` (severity `NOTICE`, with the notice's SQLSTATE: `42P07` for "relation ... already exists, skipping", `00000` for "... does not exist, skipping", as PostgreSQL), then, for a `SELECT`, `RowDescription` and one `DataRow` per row, then `CommandComplete` with the command tag.
- **If a statement failed:** `ErrorResponse` (severity `ERROR`, with the code, message, detail, hint and position the executor reports; the position counts characters in the whole query string, as PostgreSQL's does). The statements after it do not run. The statements before it stay committed (10-executor.md section 5: there are no transactions yet, so this is where NoVacDB differs from PostgreSQL's implicit transaction around a multi-statement string).
- **An empty string** (nothing but whitespace, comments and semicolons) gets `EmptyQueryResponse`.
- **Then `ReadyForQuery`** with status `I` (idle): with no transactions, the session is always idle between queries.

**RowDescription** describes each column as PostgreSQL does for a computed column: name, table OID 0, attribute number 0, the type's OID and size, type modifier −1, and format 0 (text).

| type | OID | size |
|---|---|---|
| `integer` | 23 (`int4`) | 4 |
| `bigint` | 20 (`int8`) | 8 |
| `double precision` | 701 (`float8`) | 8 |
| `text` | 25 (`text`) | −1 |
| `boolean` | 16 (`bool`) | 1 |
| `timestamptz` | 1184 (`timestamptz`) | 8 |

(PostgreSQL reports the source table and column for plain column references; clients use them only for updatable result sets, which nothing here supports. Zero is valid and means "not a column".)

**DataRow** values are the text output forms of 10-executor.md section 2.1, exactly what `psql` prints (`t`/`f`, `2024-01-02 03:04:05+00`, shortest-exact doubles); NULL is length −1.

**Results are built whole, then sent.** The executor builds a statement's rows before returning them (10-executor.md section 8), and the read lock is released before any byte goes to the client, so a slow client never holds up writers. Sending streams the built rows through the connection's buffer, flushing every 64 KiB, so the protocol side adds no second copy of a large result. Streaming rows from the executor (a cursor API) waits for Phase 7, where large results become common.

**Context.** Each query runs with a context that is cancelled when the server shuts down; Step 5.4's `CancelRequest` will cancel it too. A client that disconnects is noticed when the server next writes or reads.

**Other messages, until Step 5.3:** the extended-protocol messages (`Parse`, `Bind`, `Describe`, `Execute`, `Close`, `Flush`) get one `ERROR 0A000` "the extended query protocol is not implemented yet", after which the server discards messages until `Sync` and then sends `ReadyForQuery`, exactly as PostgreSQL recovers from an error in an extended-protocol sequence; `Sync` alone gets `ReadyForQuery`. `FunctionCall` gets `ERROR 0A000` and `ReadyForQuery`. `CopyData`, `CopyDone` and `CopyFail` outside a COPY are ignored, as PostgreSQL does. Anything else is `FATAL 08P01` as in 5.1.

**What does not work yet, and why it is not hidden:** `BEGIN`, `COMMIT`, `SET`, `SHOW` and queries on `pg_catalog` (which `psql`'s `\d` commands send) are syntax or unknown-table errors until transactions (Phase 6) and catalog compatibility (Phase 9). They fail with PostgreSQL's error codes, so clients report them clearly.

### 2.9 Implementation notes (Step 5.2)

- **Notices carry their SQLSTATE.** The executor's notices became `{Code, Message}` pairs (`42P07` for "already exists, skipping", `00000` for "does not exist, skipping"), so `NoticeResponse` has the same `C` field as PostgreSQL's.
- **`SELECT FROM t` (an empty select list) is a result.** PostgreSQL sends a `RowDescription` of no columns and one empty `DataRow` per row, and `psql` prints `--` and `(n rows)`. The executor returned no column list for it, which the server took for a statement without rows; every `SELECT` result now has a non-nil column list, empty or not.
- **At most 1664 output columns.** `RowDescription` and `DataRow` count columns in 16 bits, so a select list of 70,000 entries corrupted the stream (found by trying it with `psql`). The executor now refuses more than 1664, PostgreSQL's limit, with `54011` "target lists can have at most 1664 entries" at the first target past the limit; `*` counts every column it expands to.
- **Wide rows.** Trying a 1000-column table for the limit above found a crash from Step 4.3: encoding a row of more than 488 columns made its first buffer with a capacity smaller than its length. Fixed, with a round-trip test of rows up to 1600 columns.
- **`Config.DB` is required.** `server.New` refuses a nil database; tests use an in-memory one.
- **The SQL logic tests run over the wire too.** The runner works through a `Session` interface; besides the executor directly, every test file now runs through a server on loopback TCP with a small protocol client in the test code, so all 250 records check what the protocol carries.
- **Verified with real `psql` 16:** creating, filling, querying, updating and deleting; aligned output of every type and NULL; errors with `LINE 1:` and the caret under the position; notices; several `-c` commands; `SELECT FROM t`; the column limit; a clean stop on SIGTERM. An integration test runs these through `psql` whenever it is installed.

## 3. Formats

Messages exactly as in PostgreSQL's documentation, chapter 55.7 (protocol 3.0). Step 5.1 sends `R` (AuthenticationOk, 9 bytes: `R`, length 8, 0), `S`, `K`, `Z`, `E`, `N`, `v`, and the single byte `N` for declined encryption; it reads the startup packets of section 2.3 and the type-byte messages `X` and `Q`. Step 5.2 adds `T` (RowDescription), `D` (DataRow), `C` (CommandComplete) and `I` (EmptyQueryResponse), and reads the messages listed in 2.8. Nothing is stored on disk.

## 4. Concurrency

One goroutine per connection, plus the accept loop. Connections share only the `executor.DB` (safe for concurrent use: 10-executor.md section 2.6) and the server's table of cancel keys (a mutex-protected map). Each connection reads and writes its own socket; writes are buffered and flushed at `ReadyForQuery` and before blocking reads, so a query's messages go out in few system calls.

## 5. Failure behaviour

| Event | Result |
|---|---|
| Startup packet too short, too long, or malformed | `FATAL 08P01`, close. |
| Startup not finished within 60 s (PostgreSQL's `authentication_timeout`) | Close without a message. |
| Unsupported protocol version | `FATAL 0A000`, close. |
| Unknown or invalid startup parameter | `FATAL 42704` / `22023` / `0A000`, close. |
| Client closes or the network fails at any point | The connection's goroutine ends and releases its cancel key; the server and other connections are unaffected. |
| A message longer than 16 MiB | `FATAL 08P01` before reading its body, close. |
| Unknown message type | `FATAL 08P01`, close. |
| A statement fails | `ERROR` with its SQLSTATE; the rest of the query string does not run; the session continues. |
| The client hangs up in the middle of a result | The next write fails, the connection's goroutine ends; the statement had already finished. |
| Panic while serving a connection (a bug) | Recovered in that goroutine, logged with the stack, `FATAL XX000` sent if possible, connection closed. The server keeps running. |
| The database cannot be opened at startup | The process exits with an error before listening. |

## 6. Alternatives considered

- **TLS now.** Declining `SSLRequest` is valid protocol, and every client falls back unless told `sslmode=require`. TLS needs certificate configuration and is not what the MVP is about; the framing allows adding it (reply `S` and wrap the connection) without other changes.
- **Rejecting unknown database names.** PostgreSQL does, but NoVacDB has one database; refusing names would break `psql` with default arguments for no benefit. A later multi-database design can start refusing.
- **Ignoring unknown startup parameters.** Friendlier, but a client that asked for a setting would believe it applied. NoVacDB follows PostgreSQL and refuses, except for the few listed in 2.3 whose effect is nil or impossible to observe today.
- **`server_version` "0.5".** Honest, but drivers would conclude it is PostgreSQL 0.5 and either refuse to connect or fall back to ancient SQL.
- **A third-party protocol library.** Ruled out by the zero-dependency rule, and the protocol is small.

## 7. Testing plan (Steps 5.1 and 5.2)

- **Codec:** every backend message's bytes against hand-written golden bytes; the startup decoder on valid packets with every parameter, and on every truncation, missing terminator, trailing byte, bad length and duplicate; `FuzzStartup` (no panic, and any accepted packet re-encodes to the same parameters) and `FuzzMessageReader` (arbitrary bytes never make it allocate more than the limit or panic).
- **Handshake, over real loopback TCP against an in-process server:** plain startup; `SSLRequest` then startup; `GSSENCRequest` then `SSLRequest` then startup; a third encryption request; data pipelined after `SSLRequest`; protocol 2.0, 4.0 and 3.2 (`NegotiateProtocolVersion` listing `_pq_.` options); missing user; database defaulting to the user; each parameter rule of 2.3 including `options`; the exact sequence and values of `ParameterStatus`; distinct cancel keys across connections; `Terminate`; a `Query` before 5.2; an unknown message type; an oversized message (rejected without reading its body); a client that stalls during startup (closed after the timeout, made short in tests); abrupt disconnects at every byte of the handshake; many concurrent handshakes under `-race`.
- **A real client:** `psql -h localhost -p 5433 -c ''`-style connection check in an integration test that runs only when `psql` is installed (it is in CI's image and here), plus the manual steps in the step's notes.
- **Command:** `novacdb` starts, listens, logs its address, and exits cleanly on SIGTERM.
- **Step 5.2:** golden bytes of the new messages; every type's OID, size and text form over TCP; NULL against the empty string; several statements in one query, stopping at the first error with earlier ones kept; notices with their codes; empty queries; the extended-protocol messages skipped to `Sync`, again after each `Sync`; a 2.5 MB result in many writes and in order; clients hanging up in the middle of it; concurrent sessions updating the same rows under `-race`; pipelined queries; `psql` end to end; and all SQL logic test files through the server.

## 8. Limitations

- Trust authentication only: anyone who can reach the port is in. Hence `localhost` by default.
- No TLS or GSS encryption.
- Protocol 3.0 only (3.2's features, such as longer cancel keys, are negotiated away).
- One database per data directory; the database name in the startup message is not checked.
- UTF-8 only; session time zone UTC only; date style ISO only.
