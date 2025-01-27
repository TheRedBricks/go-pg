package pg

import (
	"bufio"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"mellium.im/sasl"

	"gopkg.in/pg.v5/internal"
	"gopkg.in/pg.v5/internal/pool"
	"gopkg.in/pg.v5/orm"
	"gopkg.in/pg.v5/types"
)

// https://www.postgresql.org/docs/current/protocol-message-formats.html
const (
	commandCompleteMsg  = 'C'
	errorResponseMsg    = 'E'
	noticeResponseMsg   = 'N'
	parameterStatusMsg  = 'S'
	authenticationOKMsg = 'R'
	backendKeyDataMsg   = 'K'
	noDataMsg           = 'n'
	passwordMessageMsg  = 'p'
	terminateMsg        = 'X'

	saslInitialResponseMsg        = 'p'
	authenticationSASLContinueMsg = 'R'
	saslResponseMsg               = 'p'
	authenticationSASLFinalMsg    = 'R'

	authenticationOK                = 0
	authenticationCleartextPassword = 3
	authenticationMD5Password       = 5
	authenticationSASL              = 10

	notificationResponseMsg = 'A'

	describeMsg             = 'D'
	parameterDescriptionMsg = 't'

	queryMsg              = 'Q'
	readyForQueryMsg      = 'Z'
	emptyQueryResponseMsg = 'I'
	rowDescriptionMsg     = 'T'
	dataRowMsg            = 'D'

	parseMsg         = 'P'
	parseCompleteMsg = '1'

	bindMsg         = 'B'
	bindCompleteMsg = '2'

	executeMsg = 'E'

	syncMsg  = 'S'
	flushMsg = 'H'

	closeMsg         = 'C'
	closeCompleteMsg = '3'

	copyInResponseMsg  = 'G'
	copyOutResponseMsg = 'H'
	copyDataMsg        = 'd'
	copyDoneMsg        = 'c'
)

func startup(cn *pool.Conn, user, password, database string) error {
	writeStartupMsg(cn.Wr, user, database)
	if err := cn.FlushWriter(); err != nil {
		return err
	}

	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return err
		}
		switch c {
		case backendKeyDataMsg:
			processId, err := readInt32(cn)
			if err != nil {
				return err
			}
			secretKey, err := readInt32(cn)
			if err != nil {
				return err
			}
			cn.ProcessId = processId
			cn.SecretKey = secretKey
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return err
			}
		case authenticationOKMsg:
			if err := authenticate(cn, user, password); err != nil {
				return err
			}
		case readyForQueryMsg:
			_, err := cn.ReadN(msgLen)
			return err
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		default:
			return fmt.Errorf("pg: unknown startup message response: %q", c)
		}
	}
}

func enableSSL(cn *pool.Conn, tlsConf *tls.Config) error {
	writeSSLMsg(cn.Wr)
	if err := cn.FlushWriter(); err != nil {
		return err
	}

	c, err := cn.Rd.ReadByte()
	if err != nil {
		return err
	}
	if c != 'S' {
		return errSSLNotSupported
	}

	cn.SetNetConn(tls.Client(cn.NetConn(), tlsConf))
	return nil
}

func authenticate(cn *pool.Conn, user, password string) error {
	num, err := readInt32(cn)
	if err != nil {
		return err
	}
	switch num {
	case authenticationOK:
		return nil
	case authenticationCleartextPassword:
		writePasswordMsg(cn.Wr, password)
		if err := cn.FlushWriter(); err != nil {
			return err
		}

		c, _, err := readMessageType(cn)
		if err != nil {
			return err
		}
		switch c {
		case authenticationOKMsg:
			num, err := readInt32(cn)
			if err != nil {
				return err
			}
			if num != 0 {
				return fmt.Errorf("pg: unexpected authentication code: %d", num)
			}
			return nil
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		default:
			return fmt.Errorf("pg: unknown password message response: %q", c)
		}
	case authenticationMD5Password:
		b, err := cn.ReadN(4)
		if err != nil {
			return err
		}

		secret := "md5" + md5s(md5s(password+user)+string(b))
		writePasswordMsg(cn.Wr, secret)
		if err := cn.FlushWriter(); err != nil {
			return err
		}

		c, _, err := readMessageType(cn)
		if err != nil {
			return err
		}
		switch c {
		case authenticationOKMsg:
			num, err := readInt32(cn)
			if err != nil {
				return err
			}
			if num != 0 {
				return fmt.Errorf("pg: unexpected authentication code: %d", num)
			}
			return nil
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		default:
			return fmt.Errorf("pg: unknown password message response: %q", c)
		}
	case authenticationSASL:
		var saslMech sasl.Mechanism
	loop:
		for {
			s, err := readString(cn)
			if err != nil {
				return err
			}

			switch s {
			case "":
				break loop
			case sasl.ScramSha256.Name:
				saslMech = sasl.ScramSha256
			case sasl.ScramSha256Plus.Name:
				// ignore
			default:
				return fmt.Errorf("got %q, wanted %q", s, sasl.ScramSha256.Name)
			}
		}

		creds := sasl.Credentials(func() (Username, Password, Identity []byte) {
			return []byte(user), []byte(password), nil
		})
		client := sasl.NewClient(saslMech, creds)

		_, resp, err := client.Step(nil)
		if err != nil {
			return err
		}

		cn.Wr.StartMessage(saslInitialResponseMsg)
		cn.Wr.WriteString(saslMech.Name)
		cn.Wr.WriteInt32(int32(len(resp)))
		_, err = cn.Wr.Write(resp)
		if err != nil {
			return err
		}
		cn.Wr.FinishMessage()
		if err := cn.FlushWriter(); err != nil {
			return err
		}

		typ, n, err := readMessageType(cn)
		if err != nil {
			return err
		}
		switch typ {
		case authenticationSASLContinueMsg:
			c11, err := readInt32(cn)
			if err != nil {
				return err
			}
			if c11 != 11 {
				return fmt.Errorf("pg: SASL: got %q, wanted %q", typ, 11)
			}
			b, err := cn.ReadN(n - 4)
			if err != nil {
				return err
			}
			_, resp, err = client.Step(b)
			if err != nil {
				return err
			}
			cn.Wr.StartMessage(saslResponseMsg)
			_, err = cn.Wr.Write(resp)
			if err != nil {
				return err
			}
			cn.Wr.FinishMessage()
			if err := cn.FlushWriter(); err != nil {
				return err
			}
			return readAuthSASLFinal(cn, client)

		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		default:
			return fmt.Errorf(
				"pg: SASL: got %q, wanted %q", typ, authenticationSASLContinueMsg)
		}

	default:
		return fmt.Errorf("pg: unknown authentication message response: %d", num)
	}
}

func md5s(s string) string {
	h := md5.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func writeStartupMsg(buf *pool.WriteBuffer, user, database string) {
	buf.StartMessage(0)
	buf.WriteInt32(196608)
	buf.WriteString("user")
	buf.WriteString(user)
	buf.WriteString("database")
	buf.WriteString(database)
	buf.WriteString("")
	buf.FinishMessage()
}

func writeSSLMsg(buf *pool.WriteBuffer) {
	buf.StartMessage(0)
	buf.WriteInt32(80877103)
	buf.FinishMessage()
}

func writePasswordMsg(buf *pool.WriteBuffer, password string) {
	buf.StartMessage(passwordMessageMsg)
	buf.WriteString(password)
	buf.FinishMessage()
}

func writeFlushMsg(buf *pool.WriteBuffer) {
	buf.StartMessage(flushMsg)
	buf.FinishMessage()
}

func writeCancelRequestMsg(buf *pool.WriteBuffer, processId, secretKey int32) {
	buf.StartMessage(0)
	buf.WriteInt32(80877102)
	buf.WriteInt32(processId)
	buf.WriteInt32(secretKey)
	buf.FinishMessage()
}

func writeQueryMsg(buf *pool.WriteBuffer, fmter orm.QueryFormatter, query interface{}, params ...interface{}) error {
	buf.StartMessage(queryMsg)
	bytes, err := appendQuery(buf.Bytes, fmter, query, params...)
	if err != nil {
		buf.Reset()
		return err
	}
	if internal.QueryLogger != nil {
		internal.LogQuery(string(bytes[5:]))
	}
	buf.Bytes = bytes
	buf.WriteByte(0x0)
	buf.FinishMessage()
	return nil
}

func appendQuery(dst []byte, fmter orm.QueryFormatter, query interface{}, params ...interface{}) ([]byte, error) {
	switch query := query.(type) {
	case orm.QueryAppender:
		return query.AppendQuery(dst, params...)
	case string:
		return fmter.FormatQuery(dst, query, params...), nil
	default:
		return nil, fmt.Errorf("pg: can't append %T", query)
	}
}

func writeSyncMsg(buf *pool.WriteBuffer) {
	buf.StartMessage(syncMsg)
	buf.FinishMessage()
}

func writeParseDescribeSyncMsg(buf *pool.WriteBuffer, name, q string) {
	buf.StartMessage(parseMsg)
	buf.WriteString(name)
	buf.WriteString(q)
	buf.WriteInt16(0)
	buf.FinishMessage()

	buf.StartMessage(describeMsg)
	buf.WriteByte('S')
	buf.WriteString(name)
	buf.FinishMessage()

	writeSyncMsg(buf)
}

func readParseDescribeSync(cn *pool.Conn) ([][]byte, error) {
	var columns [][]byte
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return nil, err
		}
		switch c {
		case parseCompleteMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
		case rowDescriptionMsg: // Response to the DESCRIBE message.
			columns, err = readRowDescription(cn, nil)
			if err != nil {
				return nil, err
			}
		case parameterDescriptionMsg: // Response to the DESCRIBE message.
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
		case noDataMsg: // Response to the DESCRIBE message.
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
		case readyForQueryMsg:
			_, err := cn.ReadN(msgLen)
			return columns, err
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return nil, err
			}
			return nil, e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return nil, err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("pg: readParseDescribeSync: unexpected message %#x", c)
		}
	}
}

// Writes BIND, EXECUTE and SYNC messages.
func writeBindExecuteMsg(buf *pool.WriteBuffer, name string, params ...interface{}) error {
	const paramLenWidth = 4

	buf.StartMessage(bindMsg)
	buf.WriteString("")
	buf.WriteString(name)
	buf.WriteInt16(0)
	buf.WriteInt16(int16(len(params)))
	for _, param := range params {
		buf.StartParam()
		bytes := types.Append(buf.Bytes, param, 0)
		if bytes != nil {
			buf.Bytes = bytes
			buf.FinishParam()
		} else {
			buf.FinishNullParam()
		}
	}
	buf.WriteInt16(0)
	buf.FinishMessage()

	buf.StartMessage(executeMsg)
	buf.WriteString("")
	buf.WriteInt32(0)
	buf.FinishMessage()

	writeSyncMsg(buf)

	return nil
}

func readBindMsg(cn *pool.Conn) error {
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return err
		}
		switch c {
		case bindCompleteMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return err
			}
		case readyForQueryMsg: // This is response to the SYNC message.
			_, err := cn.ReadN(msgLen)
			return err
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return err
			}
		default:
			return fmt.Errorf("pg: readBindMsg: unexpected message %#x", c)
		}
	}
}

func writeCloseMsg(buf *pool.WriteBuffer, name string) {
	buf.StartMessage(closeMsg)
	buf.WriteByte('S')
	buf.WriteString(name)
	buf.FinishMessage()
}

func readCloseCompleteMsg(cn *pool.Conn) error {
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return err
		}
		switch c {
		case closeCompleteMsg:
			_, err := cn.ReadN(msgLen)
			return err
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return err
			}
		default:
			return fmt.Errorf("pg: readCloseCompleteMsg: unexpected message %#x", c)
		}
	}
}

func readSimpleQuery(cn *pool.Conn) (res *types.Result, retErr error) {
	setErr := func(err error) {
		if retErr == nil {
			retErr = err
		}
	}

	var rows int
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return nil, err
		}

		switch c {
		case commandCompleteMsg:
			b, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			res = types.NewResult(b, rows)
		case readyForQueryMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			return res, retErr
		case rowDescriptionMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
		case dataRowMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			rows++
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return nil, err
			}
			setErr(e)
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return nil, err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("pg: readSimpleQuery: unexpected message %#x", c)
		}
	}
}

func readExtQuery(cn *pool.Conn) (res *types.Result, retErr error) {
	setErr := func(err error) {
		if retErr == nil {
			retErr = err
		}
	}

	var rows int
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return nil, err
		}

		switch c {
		case bindCompleteMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
		case dataRowMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			rows++
		case commandCompleteMsg: // Response to the EXECUTE message.
			b, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			res = types.NewResult(b, rows)
		case readyForQueryMsg: // Response to the SYNC message.
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			return res, retErr
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return nil, err
			}
			setErr(e)
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return nil, err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("pg: readExtQuery: unexpected message %#x", c)
		}
	}
}

func readRowDescription(cn *pool.Conn, columns [][]byte) ([][]byte, error) {
	colNum, err := readInt16(cn)
	if err != nil {
		return nil, err
	}

	columns = setByteSliceLen(columns, int(colNum))
	for i := 0; i < int(colNum); i++ {
		columns[i], err = readBytes(cn, columns[i][:0])
		if err != nil {
			return nil, err
		}
		if _, err := cn.ReadN(18); err != nil {
			return nil, err
		}
	}

	return columns, nil
}

func setByteSliceLen(b [][]byte, n int) [][]byte {
	if n <= cap(b) {
		return b[:n]
	}
	b = b[:cap(b)]
	b = append(b, make([][]byte, n-cap(b))...)
	return b
}

func readDataRow(cn *pool.Conn, scanner orm.ColumnScanner, columns [][]byte) (retErr error) {
	setErr := func(err error) {
		if retErr == nil {
			retErr = err
		}
	}

	colNum, err := readInt16(cn)
	if err != nil {
		return err
	}

	for colIdx := int16(0); colIdx < colNum; colIdx++ {
		l, err := readInt32(cn)
		if err != nil {
			return err
		}

		var b []byte
		if l != -1 { // NULL
			b, err = cn.ReadN(int(l))
			if err != nil {
				return err
			}
		}

		column := internal.BytesToString(columns[colIdx])
		if err := scanner.ScanColumn(int(colIdx), column, b); err != nil {
			setErr(err)
		}

	}

	return retErr
}

func newModel(mod interface{}) (orm.Model, error) {
	m, ok := mod.(orm.Model)
	if ok {
		return m, m.Reset()
	}

	m, err := orm.NewModel(mod)
	if err != nil {
		return nil, err
	}
	return m, m.Reset()
}

func readSimpleQueryData(
	cn *pool.Conn, mod interface{},
) (res *types.Result, model orm.Model, retErr error) {
	setErr := func(err error) {
		if retErr == nil {
			retErr = err
		}
	}

	var rows int
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return nil, nil, err
		}

		switch c {
		case rowDescriptionMsg:
			cn.Columns, err = readRowDescription(cn, cn.Columns[:0])
			if err != nil {
				return nil, nil, err
			}

			if model == nil {
				var err error
				model, err = newModel(mod)
				if err != nil {
					setErr(err)
					model = Discard
				}
			}
		case dataRowMsg:
			m := model.NewModel()
			if err := readDataRow(cn, m, cn.Columns); err != nil {
				setErr(err)
			} else {
				if err := model.AddModel(m); err != nil {
					setErr(err)
				}
			}

			rows++
		case commandCompleteMsg:
			b, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, nil, err
			}
			res = types.NewResult(b, rows)
		case readyForQueryMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, nil, err
			}
			return res, model, retErr
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return nil, nil, err
			}
			setErr(e)
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return nil, nil, err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, fmt.Errorf("pg: readSimpleQueryData: unexpected message %#x", c)
		}
	}
}

func readExtQueryData(
	cn *pool.Conn, mod interface{}, columns [][]byte,
) (res *types.Result, model orm.Model, retErr error) {
	setErr := func(err error) {
		if retErr == nil {
			retErr = err
		}
	}

	var rows int
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return nil, nil, err
		}

		switch c {
		case bindCompleteMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, nil, err
			}
		case dataRowMsg:
			if model == nil {
				var err error
				model, err = newModel(mod)
				if err != nil {
					setErr(err)
					model = Discard
				}
			}

			m := model.NewModel()
			if err := readDataRow(cn, m, columns); err != nil {
				setErr(err)
			} else {
				if err := model.AddModel(m); err != nil {
					setErr(err)
				}
			}

			rows++
		case commandCompleteMsg: // Response to the EXECUTE message.
			b, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, nil, err
			}
			res = types.NewResult(b, rows)
		case readyForQueryMsg: // Response to the SYNC message.
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, nil, err
			}
			return res, model, retErr
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return nil, nil, err
			}
			setErr(e)
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return nil, nil, err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, fmt.Errorf("pg: readExtQueryData: unexpected message %#x", c)
		}
	}
}

func readCopyInResponse(cn *pool.Conn) error {
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return err
		}

		switch c {
		case copyInResponseMsg:
			_, err := cn.ReadN(msgLen)
			return err
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return err
			}
		default:
			return fmt.Errorf("pg: readCopyInResponse: unexpected message %#x", c)
		}
	}
}

func readCopyOutResponse(cn *pool.Conn) error {
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return err
		}

		switch c {
		case copyOutResponseMsg:
			_, err := cn.ReadN(msgLen)
			return err
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return err
			}
			return e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return err
			}
		default:
			return fmt.Errorf("pg: readCopyOutResponse: unexpected message %#x", c)
		}
	}
}

func readCopyData(cn *pool.Conn, w io.Writer) (*types.Result, error) {
	var res *types.Result
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return nil, err
		}

		switch c {
		case copyDataMsg:
			b, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}

			_, err = w.Write(b)
			if err != nil {
				return nil, err
			}
		case copyDoneMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
		case commandCompleteMsg:
			b, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			res = types.NewResult(b, 0)
		case readyForQueryMsg:
			_, err := cn.ReadN(msgLen)
			return res, err
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return nil, err
			}
			return nil, e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return nil, err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("pg: readCopyData: unexpected message %#x", c)
		}
	}
}

func writeCopyData(buf *pool.WriteBuffer, r io.Reader) (int64, error) {
	buf.StartMessage(copyDataMsg)
	n, err := buf.ReadFrom(r)
	buf.FinishMessage()
	return n, err
}

func writeCopyDone(buf *pool.WriteBuffer) {
	buf.StartMessage(copyDoneMsg)
	buf.FinishMessage()
}

func readReadyForQuery(cn *pool.Conn) (res *types.Result, retErr error) {
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return nil, err
		}

		switch c {
		case commandCompleteMsg:
			b, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			res = types.NewResult(b, 0)
		case readyForQueryMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return nil, err
			}
			return res, retErr
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return nil, err
			}
			retErr = e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return nil, err
			}
		case parameterStatusMsg:
			if err := logParameterStatus(cn, msgLen); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("pg: readReadyForQueryOrError: unexpected message %#x", c)
		}
	}
}

func readNotification(cn *pool.Conn) (channel, payload string, err error) {
	for {
		c, msgLen, err := readMessageType(cn)
		if err != nil {
			return "", "", err
		}

		switch c {
		case commandCompleteMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return "", "", err
			}
		case readyForQueryMsg:
			_, err := cn.ReadN(msgLen)
			if err != nil {
				return "", "", err
			}
		case errorResponseMsg:
			e, err := readError(cn)
			if err != nil {
				return "", "", err
			}
			return "", "", e
		case noticeResponseMsg:
			if err := logNotice(cn, msgLen); err != nil {
				return "", "", err
			}
		case notificationResponseMsg:
			_, err := readInt32(cn)
			if err != nil {
				return "", "", err
			}
			channel, err = readString(cn)
			if err != nil {
				return "", "", err
			}
			payload, err = readString(cn)
			if err != nil {
				return "", "", err
			}
			return channel, payload, nil
		default:
			return "", "", fmt.Errorf("pg: unexpected message %q", c)
		}
	}
}

var terminateMessage = []byte{terminateMsg, 0, 0, 0, 4}

func terminateConn(cn *pool.Conn) error {
	// Don't use cn.Buf because it is racy with user code.
	_, err := cn.NetConn().Write(terminateMessage)
	return err
}

//------------------------------------------------------------------------------

func readInt16(cn *pool.Conn) (int16, error) {
	b, err := cn.ReadN(2)
	if err != nil {
		return 0, err
	}
	return int16(binary.BigEndian.Uint16(b)), nil
}

func readInt32(cn *pool.Conn) (int32, error) {
	b, err := cn.ReadN(4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}

func readString(cn *pool.Conn) (string, error) {
	s, err := cn.Rd.ReadString(0)
	if err != nil {
		return "", err
	}
	return s[:len(s)-1], nil
}

func readBytes(cn *pool.Conn, b []byte) ([]byte, error) {
	for {
		line, err := cn.Rd.ReadSlice(0)
		if err != nil && err != bufio.ErrBufferFull {
			return nil, err
		}
		b = append(b, line...)
		if err == nil {
			break
		}
	}
	return b[:len(b)-1], nil
}

func readError(cn *pool.Conn) (error, error) {
	m := map[byte]string{
		'a': cn.RemoteAddr().String(),
	}
	for {
		c, err := cn.Rd.ReadByte()
		if err != nil {
			return nil, err
		}
		if c == 0 {
			break
		}
		s, err := readString(cn)
		if err != nil {
			return nil, err
		}
		m[c] = s
	}

	return internal.NewPGError(m), nil
}

func readMessageType(cn *pool.Conn) (byte, int, error) {
	c, err := cn.Rd.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	l, err := readInt32(cn)
	if err != nil {
		return 0, 0, err
	}
	return c, int(l) - 4, nil
}

func logNotice(cn *pool.Conn, msgLen int) error {
	_, err := cn.ReadN(msgLen)
	return err
}

func logParameterStatus(cn *pool.Conn, msgLen int) error {
	_, err := cn.ReadN(msgLen)
	return err
}

func readAuthSASLFinal(cn *pool.Conn, client *sasl.Negotiator) error {
	c, n, err := readMessageType(cn)
	if err != nil {
		return err
	}

	switch c {
	case authenticationSASLFinalMsg:
		c12, err := readInt32(cn)
		if err != nil {
			return err
		}
		if c12 != 12 {
			return fmt.Errorf("pg: SASL: got %q, wanted %q", c, 12)
		}

		b, err := cn.ReadN(n - 4)
		if err != nil {
			return err
		}

		_, _, err = client.Step(b)
		if err != nil {
			return err
		}

		if client.State() != sasl.ValidServerResponse {
			return fmt.Errorf("pg: SASL: state=%q, wanted %q",
				client.State(), sasl.ValidServerResponse)
		}
	case errorResponseMsg:
		e, err := readError(cn)
		if err != nil {
			return err
		}
		return e
	default:
		return fmt.Errorf(
			"pg: SASL: got %q, wanted %q", c, authenticationSASLFinalMsg)
	}

	return readAuthOK(cn)
}

func readAuthOK(cn *pool.Conn) error {
	c, _, err := readMessageType(cn)
	if err != nil {
		return err
	}

	switch c {
	case authenticationOKMsg:
		c0, err := readInt32(cn)
		if err != nil {
			return err
		}
		if c0 != 0 {
			return fmt.Errorf("pg: unexpected authentication code: %q", c0)
		}
		return nil
	case errorResponseMsg:
		e, err := readError(cn)
		if err != nil {
			return err
		}
		return e
	default:
		return fmt.Errorf("pg: unknown password message response: %q", c)
	}
}
