package server

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
)

// conn is one client connection.
type conn struct {
	s   *Server
	c   net.Conn
	br  *bufio.Reader
	out pgwire.Buffer
	log *slog.Logger

	stmts   map[string]*statement // prepared statements, by name
	portals map[string]*portal    // portals, by name
}

// handle serves a connection until it ends. A panic is contained here: it
// is logged, the client is told if possible, and only this connection
// closes.
func (s *Server) handle(nc net.Conn) {
	cn := &conn{s: s, c: nc, br: bufio.NewReader(nc), log: s.log.With("remote", nc.RemoteAddr().String())}
	defer s.untrack(nc)
	defer func() { _ = nc.Close() }()
	defer func() {
		if r := recover(); r != nil {
			cn.log.Error("panic while serving a connection", "panic", r, "stack", string(debug.Stack()))
			cn.out.Reset()
			cn.fatal(sqlerr.New(sqlerr.InternalError, "internal error"))
		}
	}()
	_ = nc.SetDeadline(time.Now().Add(s.cfg.StartupTimeout))
	sess, err := cn.startup()
	if err != nil {
		var se *sqlerr.Error
		if errors.As(err, &se) {
			cn.log.Info("connection refused", "code", se.Code, "err", se.Message)
			cn.fatal(se)
		} else {
			cn.log.Debug("connection ended during startup", "err", err)
		}
		return
	}
	if sess == nil {
		return // a CancelRequest: nothing to answer
	}
	defer s.unregister(sess.pid)
	_ = nc.SetDeadline(time.Time{})
	cn.log = cn.log.With("pid", sess.pid, "user", sess.user)
	cn.log.Info("session started", "database", sess.database, "application_name", sess.applicationName)
	if len(sess.ignored) > 0 {
		cn.log.Info("startup settings accepted and ignored", "settings", strings.Join(sess.ignored, ","))
	}
	s.mu.Lock()
	hook := s.afterStartup
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	err = cn.serve()
	var se *sqlerr.Error
	switch {
	case err == nil:
		cn.log.Info("session ended")
	case errors.As(err, &se):
		cn.log.Info("session ended by an error", "code", se.Code, "err", se.Message)
		cn.fatal(se)
	default:
		cn.log.Info("session ended", "err", err)
	}
}

// flush writes the buffered messages.
func (cn *conn) flush() error {
	if len(cn.out.Bytes()) == 0 {
		return nil
	}
	_, err := cn.c.Write(cn.out.Bytes())
	cn.out.Reset()
	return err
}

// fatal sends a FATAL error; the caller then closes the connection.
func (cn *conn) fatal(e *sqlerr.Error) {
	cn.out.ErrorResponse(errorFields(pgwire.SeverityFatal, e))
	_ = cn.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = cn.flush()
}

func errorFields(severity string, e *sqlerr.Error) pgwire.ErrorFields {
	return pgwire.ErrorFields{Severity: severity, Code: e.Code, Message: e.Message, Detail: e.Detail, Hint: e.Hint, Position: e.Position}
}

// protocolViolation turns a framing error into the FATAL 08P01 the client
// is sent.
func protocolViolation(err error) *sqlerr.Error {
	return sqlerr.Wrap(err, sqlerr.ProtocolViolation, "%s", strings.TrimSuffix(err.Error(), ": "+pgwire.ErrProtocol.Error()))
}

// startedSession is a session that completed the startup handshake.
type startedSession struct {
	session
	pid uint32
}

// startup runs the handshake of design doc section 2.3. It returns the
// session, nil for a CancelRequest, or an error: a *sqlerr.Error to send as
// FATAL, or a network error.
func (cn *conn) startup() (*startedSession, error) {
	var declined [2]bool // SSL, GSS
	for {
		code, body, err := pgwire.ReadStartupPacket(cn.br)
		if err != nil {
			if errors.Is(err, pgwire.ErrProtocol) {
				return nil, protocolViolation(err)
			}
			return nil, err
		}
		switch code {
		case pgwire.CodeSSLRequest, pgwire.CodeGSSENCRequest:
			i := 0
			if code == pgwire.CodeGSSENCRequest {
				i = 1
			}
			if len(body) != 0 {
				return nil, sqlerr.New(sqlerr.ProtocolViolation, "invalid length of startup packet")
			}
			if declined[i] {
				return nil, sqlerr.New(sqlerr.ProtocolViolation, "duplicate encryption request")
			}
			declined[i] = true
			// A client must wait for the answer before sending more: data
			// already here may have been injected by a man in the middle.
			if cn.br.Buffered() > 0 {
				return nil, sqlerr.New(sqlerr.ProtocolViolation, "received unencrypted data after an encryption request").
					WithDetail("This could be either a client-software bug or evidence of an attempted man-in-the-middle attack.")
			}
			cn.out.EncryptionDenied()
			if err := cn.flush(); err != nil {
				return nil, err
			}
		case pgwire.CodeCancelRequest:
			return nil, nil // Step 5.4; a cancel request never gets a reply
		default:
			return cn.startSession(code, body)
		}
	}
}

func (cn *conn) startSession(code uint32, body []byte) (*startedSession, error) {
	if code>>16 != 3 {
		return nil, sqlerr.New(sqlerr.FeatureNotSupported, "unsupported frontend protocol %d.%d: server supports 3.0 to 3.0", code>>16, code&0xffff)
	}
	msg, err := pgwire.ParseStartup(code, body)
	if err != nil {
		return nil, protocolViolation(err)
	}
	sess := &startedSession{session: session{clientEncoding: "UTF8"}}
	var unrecognised []string
	for _, p := range msg.Params {
		var perr *sqlerr.Error
		switch {
		case p.Name == "user":
			sess.user = p.Value
		case p.Name == "database":
			sess.database = p.Value
		case p.Name == "options":
			perr = sess.applyOptions(p.Value)
		case p.Name == "replication":
			perr = sqlerr.New(sqlerr.FeatureNotSupported, "replication is not supported")
		case strings.HasPrefix(p.Name, "_pq_."):
			unrecognised = append(unrecognised, p.Name)
		default:
			perr = sess.setParam(p.Name, p.Value)
		}
		if perr != nil {
			return nil, perr
		}
	}
	if sess.user == "" {
		return nil, sqlerr.New(sqlerr.InvalidAuthorization, "no PostgreSQL user name specified in startup packet")
	}
	if sess.database == "" {
		sess.database = sess.user
	}
	if msg.Minor > 0 || len(unrecognised) > 0 {
		cn.out.NegotiateProtocolVersion(0, unrecognised)
	}
	pid, secret, err := cn.s.register()
	if err != nil {
		return nil, err
	}
	sess.pid = pid
	cn.out.AuthenticationOk()
	for _, p := range sess.parameterStatus(cn.s.cfg.ServerVersion) {
		cn.out.ParameterStatus(p.Name, p.Value)
	}
	cn.out.BackendKeyData(pid, secret)
	cn.out.ReadyForQuery(pgwire.StatusIdle)
	if err := cn.flush(); err != nil {
		cn.s.unregister(pid)
		return nil, err
	}
	return sess, nil
}

// parameterStatus lists what the server reports after authentication, in
// PostgreSQL 16's order (design doc section 2.3).
func (s *session) parameterStatus(serverVersion string) []pgwire.Param {
	return []pgwire.Param{
		{Name: "application_name", Value: s.applicationName},
		{Name: "client_encoding", Value: s.clientEncoding},
		{Name: "DateStyle", Value: "ISO, MDY"},
		{Name: "default_transaction_read_only", Value: "off"},
		{Name: "in_hot_standby", Value: "off"},
		{Name: "integer_datetimes", Value: "on"},
		{Name: "IntervalStyle", Value: "postgres"},
		{Name: "is_superuser", Value: "on"},
		{Name: "server_encoding", Value: "UTF8"},
		{Name: "server_version", Value: serverVersion},
		{Name: "session_authorization", Value: s.user},
		{Name: "standard_conforming_strings", Value: "on"},
		{Name: "TimeZone", Value: "UTC"},
	}
}

// serve runs the session after startup (design doc sections 2.8 and
// 2.9). It returns nil when the client terminates, a *sqlerr.Error to send
// as FATAL, or a network error.
func (cn *conn) serve() error {
	rd := pgwire.NewReader(cn.br)
	cn.stmts, cn.portals = map[string]*statement{}, map[string]*portal{}
	skipToSync := false // after an error in an extended-protocol sequence
	for {
		// Output waits while more input is already here, so a pipeline's
		// answers go out together, but never past the flush size.
		if cn.br.Buffered() == 0 || cn.out.Len() >= flushAt {
			if err := cn.flush(); err != nil {
				return err
			}
		}
		typ, body, err := rd.ReadMessage()
		if err != nil {
			if errors.Is(err, pgwire.ErrProtocol) {
				return protocolViolation(err)
			}
			return err
		}
		switch typ {
		case 'X': // Terminate
			return nil
		case 'Q': // Query
			sql, err := pgwire.ParseQuery(body)
			if err != nil {
				return protocolViolation(err)
			}
			// A simple query ends the implicit transaction and replaces the
			// unnamed statement, as in PostgreSQL.
			cn.endTransaction()
			delete(cn.stmts, "")
			if err := cn.query(sql); err != nil {
				return err
			}
			cn.out.ReadyForQuery(pgwire.StatusIdle)
		case 'P', 'B', 'D', 'E', 'C': // Parse, Bind, Describe, Execute, Close
			if skipToSync {
				continue
			}
			if err := cn.extended(typ, body); err != nil {
				var se *sqlerr.Error
				if !errors.As(err, &se) {
					return err
				}
				cn.out.ErrorResponse(errorFields(pgwire.SeverityError, se))
				skipToSync = true
			}
		case 'H': // Flush
			if err := cn.flush(); err != nil {
				return err
			}
		case 'S': // Sync
			skipToSync = false
			cn.endTransaction()
			cn.out.ReadyForQuery(pgwire.StatusIdle)
		case 'F': // FunctionCall
			cn.out.ErrorResponse(errorFields(pgwire.SeverityError,
				sqlerr.New(sqlerr.FeatureNotSupported, "function calls through the protocol are not supported")))
			cn.out.ReadyForQuery(pgwire.StatusIdle)
		case 'd', 'c', 'f': // CopyData, CopyDone, CopyFail outside a COPY: ignored, as PostgreSQL does
		default:
			return sqlerr.New(sqlerr.ProtocolViolation, "invalid frontend message type %d", typ)
		}
	}
}

// flushAt is how much output is buffered before it is written, while a
// result is being sent.
const flushAt = 64 << 10

// query runs a Query message's statements and sends their results (design
// doc section 2.8). A statement's error is sent as an ErrorResponse and
// ends the query; only a failure to write to the client is returned.
func (cn *conn) query(sql string) error {
	results, err := cn.s.cfg.DB.Exec(cn.s.ctx, sql)
	if len(results) == 0 && err == nil {
		cn.out.EmptyQueryResponse()
		return nil
	}
	for _, r := range results {
		cn.notices(r)
		if r.Columns != nil {
			cn.rowDescription(r.Columns, nil)
			if err := cn.dataRows(r.Rows, nil); err != nil {
				return err
			}
		}
		cn.out.CommandComplete(r.Tag)
	}
	if err != nil {
		cn.out.ErrorResponse(errorFields(pgwire.SeverityError, sqlerr.From(err)))
	}
	return nil
}

// notices sends a statement's notices.
func (cn *conn) notices(r *executor.Result) {
	for _, n := range r.Notices {
		cn.out.NoticeResponse(pgwire.ErrorFields{Severity: pgwire.SeverityNotice, Code: n.Code, Message: n.Message})
	}
}

// dataRows sends rows in formats (nil: all text), writing out the buffer
// whenever it grows past flushAt.
func (cn *conn) dataRows(rows [][]types.Value, formats []int16) error {
	var vals [][]byte
	buf := make([]byte, 0, 64) // never nil: an empty value is not NULL
	for _, row := range rows {
		vals, buf = vals[:0], buf[:0]
		ends := make([]int, 0, len(row))
		for i, v := range row {
			if v.Null {
				ends = append(ends, -1)
				continue
			}
			f := int16(pgwire.FormatText)
			if formats != nil {
				f = formats[i]
			}
			buf = appendValue(buf, v, f)
			ends = append(ends, len(buf))
		}
		start := 0
		for _, end := range ends {
			if end < 0 {
				vals = append(vals, nil)
				continue
			}
			vals = append(vals, buf[start:end:end])
			start = end
		}
		cn.out.DataRow(vals)
		if cn.out.Len() >= flushAt {
			if err := cn.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}
