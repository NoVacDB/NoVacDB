package server

import (
	"strconv"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
)

// statement is a prepared statement of a session (design doc section
// 2.9).
type statement struct {
	p *executor.Prepared
	// oids are the parameters' type OIDs as the client sees them: as it
	// declared them, or the inferred type's. They decide binary forms.
	oids []uint32
}

// portal is a bound statement, ready to run. It runs at its first Execute;
// a SELECT's rows are kept and handed out over as many Executes as its
// row limits need.
type portal struct {
	st      *statement
	values  []types.Value
	formats []int16 // one per result column
	ran     bool
	result  *executor.Result
	next    int // the next row to send
}

func (cn *conn) statement(name string) (*statement, error) {
	if st, ok := cn.stmts[name]; ok {
		return st, nil
	}
	if name == "" {
		return nil, sqlerr.New(sqlerr.InvalidSQLStatementName, "unnamed prepared statement does not exist")
	}
	return nil, sqlerr.New(sqlerr.InvalidSQLStatementName, "prepared statement %q does not exist", name)
}

func (cn *conn) portal(name string) (*portal, error) {
	if p, ok := cn.portals[name]; ok {
		return p, nil
	}
	return nil, sqlerr.New(sqlerr.InvalidCursorName, "portal %q does not exist", name)
}

// endTransaction ends the implicit transaction that Sync and Query end:
// every portal goes, as PostgreSQL's portals end with their transaction.
func (cn *conn) endTransaction() { clear(cn.portals) }

// extended handles one extended-protocol message. An error it returns is a
// *sqlerr.Error to send as ERROR (after which messages are skipped up to
// Sync), or a network error.
func (cn *conn) extended(typ byte, body []byte) error {
	switch typ {
	case 'P':
		m, err := pgwire.ParseParse(body)
		if err != nil {
			return protocolViolation(err)
		}
		return cn.parse(m)
	case 'B':
		m, err := pgwire.ParseBind(body)
		if err != nil {
			return protocolViolation(err)
		}
		return cn.bind(m)
	case 'D':
		m, err := pgwire.ParseDescribe(body, "Describe")
		if err != nil {
			return protocolViolation(err)
		}
		return cn.describe(m)
	case 'E':
		m, err := pgwire.ParseExecute(body)
		if err != nil {
			return protocolViolation(err)
		}
		return cn.execute(m)
	case 'C':
		m, err := pgwire.ParseDescribe(body, "Close")
		if err != nil {
			return protocolViolation(err)
		}
		if m.Kind == 'S' {
			delete(cn.stmts, m.Name)
		} else {
			delete(cn.portals, m.Name)
		}
		cn.out.CloseComplete()
	}
	return nil
}

func (cn *conn) parse(m pgwire.Parse) error {
	if _, ok := cn.stmts[m.Name]; ok && m.Name != "" {
		return sqlerr.New(sqlerr.DuplicatePreparedStatement, "prepared statement %q already exists", m.Name)
	}
	declared := make([]types.Type, len(m.ParamTypes))
	for i, oid := range m.ParamTypes {
		t, ok := paramType(oid)
		if !ok {
			return sqlerr.New(sqlerr.FeatureNotSupported, "parameter $%d has type OID %d, which is not supported", i+1, oid)
		}
		declared[i] = t
	}
	p, err := cn.s.cfg.DB.Prepare(cn.s.ctx, m.Query, declared)
	if err != nil {
		return err
	}
	st := &statement{p: p, oids: make([]uint32, len(p.ParamTypes))}
	for i, t := range p.ParamTypes {
		if i < len(m.ParamTypes) && m.ParamTypes[i] != 0 && m.ParamTypes[i] != oidUnknown {
			st.oids[i] = m.ParamTypes[i]
		} else {
			st.oids[i], _ = typeInfo(t)
		}
	}
	cn.stmts[m.Name] = st
	cn.out.ParseComplete()
	return nil
}

// format returns the format code for item i of n from a Bind's list of
// zero, one or n codes.
func format(codes []int16, i int) int16 {
	switch len(codes) {
	case 0:
		return pgwire.FormatText
	case 1:
		return codes[0]
	}
	return codes[i]
}

func (cn *conn) bind(m pgwire.Bind) error {
	st, err := cn.statement(m.Statement)
	if err != nil {
		return err
	}
	if _, ok := cn.portals[m.Portal]; ok && m.Portal != "" {
		return sqlerr.New(sqlerr.DuplicateCursor, "cursor %q already exists", m.Portal)
	}
	p := st.p
	if len(m.Params) != len(p.ParamTypes) {
		return sqlerr.New(sqlerr.ProtocolViolation, "bind message supplies %d parameters, but prepared statement %q requires %d",
			len(m.Params), m.Statement, len(p.ParamTypes))
	}
	values := make([]types.Value, len(m.Params))
	for i, data := range m.Params {
		if values[i], err = decodeParam(i+1, data, format(m.ParamFormats, i), st.oids[i], p.ParamTypes[i]); err != nil {
			return err
		}
	}
	if k := len(m.ResultFormats); k > 1 && k != len(p.Columns) {
		return sqlerr.New(sqlerr.ProtocolViolation, "bind message has %d result formats but query has %d columns", k, len(p.Columns))
	}
	formats := make([]int16, len(p.Columns))
	for i := range formats {
		formats[i] = format(m.ResultFormats, i)
	}
	for _, f := range m.ResultFormats {
		if f != pgwire.FormatText && f != pgwire.FormatBinary {
			return sqlerr.New(sqlerr.InvalidParameterValue, "unsupported format code: %d", f)
		}
	}
	cn.portals[m.Portal] = &portal{st: st, values: values, formats: formats}
	cn.out.BindComplete()
	return nil
}

func (cn *conn) describe(m pgwire.Describe) error {
	if m.Kind == 'S' {
		st, err := cn.statement(m.Name)
		if err != nil {
			return err
		}
		cn.out.ParameterDescription(st.oids)
		cn.rowDescription(st.p.Columns, nil)
		return nil
	}
	p, err := cn.portal(m.Name)
	if err != nil {
		return err
	}
	cn.rowDescription(p.st.p.Columns, p.formats)
	return nil
}

// rowDescription sends RowDescription for columns in formats (nil: text),
// or NoData for a statement without a result.
func (cn *conn) rowDescription(cols []executor.Column, formats []int16) {
	if cols == nil {
		cn.out.NoData()
		return
	}
	fields := make([]pgwire.FieldDescription, len(cols))
	for i, c := range cols {
		oid, size := typeInfo(c.Type)
		fields[i] = pgwire.FieldDescription{Name: c.Name, TypeOID: oid, Size: size}
		if formats != nil {
			fields[i].Format = formats[i]
		}
	}
	cn.out.RowDescription(fields)
}

func (cn *conn) execute(m pgwire.Execute) error {
	p, err := cn.portal(m.Portal)
	if err != nil {
		return err
	}
	if p.st.p.Empty() {
		cn.out.EmptyQueryResponse()
		return nil
	}
	first := !p.ran
	if first {
		r, err := cn.s.cfg.DB.ExecPrepared(cn.s.ctx, p.st.p, p.values)
		if err != nil {
			return err
		}
		p.ran, p.result = true, r
		cn.notices(r)
	}
	r := p.result
	if r.Columns == nil {
		if !first {
			return sqlerr.New(sqlerr.ObjectNotInPrerequisiteState, "portal %q cannot be run", m.Portal)
		}
		cn.out.CommandComplete(r.Tag)
		return nil
	}
	end := len(r.Rows)
	if m.MaxRows > 0 && int64(end-p.next) > int64(m.MaxRows) {
		end = p.next + int(m.MaxRows)
	}
	sent := end - p.next
	if err := cn.dataRows(r.Rows[p.next:end], p.formats); err != nil {
		return err
	}
	p.next = end
	if end < len(r.Rows) {
		cn.out.PortalSuspended()
		return nil
	}
	cn.out.CommandComplete("SELECT " + strconv.Itoa(sent))
	return nil
}
