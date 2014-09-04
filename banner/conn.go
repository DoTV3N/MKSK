package banner

import (
	"../zcrypto/ztls"
	"net"
	"fmt"
	"time"
	"regexp"
)

var smtpEndRegex = regexp.MustCompile(`(?:\r\n)|^[0-9]{3} .+\r\n$`)
var pop3EndRegex = regexp.MustCompile(`(?:\r\n\.\r\n$)|(?:\r\n$)`)
var imapStatusEndRegex = regexp.MustCompile(`\r\n$`)

const SMTP_COMMAND = "STARTTLS\r\n"
const POP3_COMMAND = "STLS\r\n"
const IMAP_COMMAND = "a001 STARTTLS\r\n"

// Implements the net.Conn interface
type Conn struct {
	// Underlying network connection
	conn net.Conn
	tlsConn *ztls.Conn
	isTls bool

	// Keep track of state / network operations
	operations []ConnectionOperation

	// Cache the deadlines so we can reapply after TLS handshake
	readDeadline time.Time
	writeDeadline time.Time
}

func (c *Conn) getUnderlyingConn() (net.Conn) {
	if c.isTls {
		return c.tlsConn
	}
	return c.conn
}

// Layer in the regular conn methods
func (c *Conn) LocalAddr() net.Addr {
	return c.getUnderlyingConn().LocalAddr()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.getUnderlyingConn().RemoteAddr()
}

func (c *Conn) SetDeadline(t time.Time) error {
	c.readDeadline = t
	c.writeDeadline = t
	return c.getUnderlyingConn().SetDeadline(t)
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return c.getUnderlyingConn().SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return c.getUnderlyingConn().SetWriteDeadline(t)
}

// Delegate here, but record all the things
func (c *Conn) Write(b []byte) (int, error) {
	n, err := c.getUnderlyingConn().Write(b)
	ws := writeState{toSend: b, err: err}
	c.operations = append(c.operations, &ws)
	return n, err
}

func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.getUnderlyingConn().Read(b)
	rs := readState{response: b[0:n], err: err}
	c.operations = append(c.operations, &rs)
	return n, err
}

func (c *Conn) Close() error {
	return c.getUnderlyingConn().Close()
}

// Extra method - Do a TLS Handshake and record progress
func (c *Conn) TlsHandshake() error {
	if c.isTls {
		return fmt.Errorf(
			"Attempted repeat handshake with remote host %s",
			c.RemoteAddr().String())
	}
	tlsConfig := new(ztls.Config)
	tlsConfig.InsecureSkipVerify = true
	tlsConfig.MinVersion = ztls.VersionSSL30
	c.tlsConn = ztls.Client(c.conn, tlsConfig)
	c.tlsConn.SetReadDeadline(c.readDeadline)
	c.tlsConn.SetWriteDeadline(c.writeDeadline)
	c.isTls = true
	err := c.tlsConn.Handshake()
	hl := c.tlsConn.HandshakeLog()
	ts := tlsState{handshake: hl, err: err}
	c.operations = append(c.operations, &ts)
	return err
}

func (c *Conn) sendStarttlsCommand(command string) error {
	// Don't doublehandshake
	if c.isTls {
		return fmt.Errorf(
			"Attempt STARTTLS after TLS handshake with remote host %s",
			c.RemoteAddr().String())
	}
	// Send the STARTTLS message
	starttls := []byte(command);
	_, err := c.conn.Write(starttls);
	return err
}

// Do a STARTTLS handshake
func (c *Conn) SmtpStarttlsHandshake() error {
	// Make the state
	ss := starttlsState{command: []byte(SMTP_COMMAND)}
	// Send the command
	ss.err = c.sendStarttlsCommand(SMTP_COMMAND)
	// Read the response on a successful send
	if ss.err == nil {
		buf := make([]byte, 256)
		n, err := c.readSmtpResponse(buf)
		ss.response = buf[0:n]
		ss.err = err
	}
	// No matter what happened, record the state
	c.operations = append(c.operations, &ss)
	// Stop if we failed already
	if ss.err != nil {
		return ss.err
	}
	// Successful so far, attempt to do the actual handshake
	return c.TlsHandshake()
}

func (c *Conn) Pop3StarttlsHandshake() error {
	ss := starttlsState{command: []byte(POP3_COMMAND)}
	ss.err = c.sendStarttlsCommand(POP3_COMMAND)
	if ss.err == nil {
		buf := make([]byte, 512)
		n, err := c.readPop3Response(buf)
		ss.response = buf[0:n]
		ss.err = err
	}
	c.operations = append(c.operations, &ss)
	if ss.err != nil {
		return ss.err
	}
	return c.TlsHandshake()
}

func (c *Conn) ImapStarttlsHandshake() error {
	ss := starttlsState{command: []byte(IMAP_COMMAND)}
	ss.err = c.sendStarttlsCommand(IMAP_COMMAND)
	if ss.err == nil {
		buf := make([]byte, 512)
		n, err := c.readImapStatusResponse(buf)
		ss.response = buf[0:n]
		ss.err = err
	}
	c.operations = append(c.operations, &ss)
	if ss.err != nil {
		return ss.err
	}
	return c.TlsHandshake()
}

func (c *Conn) readUntilRegex(res []byte, expr *regexp.Regexp) (int, error) {
	buf := res[0:]
	length := 0
	for finished := false; !finished; {
		n, err := c.getUnderlyingConn().Read(buf);
		length += n
		if err != nil {
			return length, err
		}
		if expr.Match(res[0:length]) {
			finished = true
		}
		if length == len(res) {
			b := make([]byte, 3*length)
			copy(b, res)
			res = b
		}
		buf = res[length:]
	}
	return length, nil
}

func (c *Conn) readSmtpResponse(res []byte) (int, error) {
	return c.readUntilRegex(res, smtpEndRegex)
}

func (c *Conn) SmtpBanner(b []byte) (int, error) {
	n, err := c.readSmtpResponse(b)
	rs := readState{}
	rs.response = b[0:n]
	rs.err = err
	c.operations = append(c.operations, &rs)
	return n, err
}

func (c *Conn) Ehlo(domain string) error {
	cmd := []byte("EHLO " + domain + "\r\n")
	es := ehloState{}
	_, writeErr := c.getUnderlyingConn().Write(cmd)
	if writeErr != nil {
		es.err = writeErr
	} else {
		buf := make([]byte, 512)
		n, readErr := c.readSmtpResponse(buf)
		es.err = readErr
		es.response = buf[0:n]
	}
	c.operations = append(c.operations, &es)
	return es.err
}

func (c *Conn) SmtpHelp() error {
	cmd := []byte("HELP\r\n")
	hs := helpState{}
	_, writeErr := c.getUnderlyingConn().Write(cmd)
	if writeErr != nil {
		hs.err = writeErr
	} else {
		buf := make([]byte, 512)
		n, readErr := c.readSmtpResponse(buf)
		hs.err = readErr
		hs.response = buf[0:n]
	}
	c.operations = append(c.operations, &hs)
	return hs.err
}

func (c *Conn) readPop3Response(res []byte) (int, error) {
	return c.readUntilRegex(res, pop3EndRegex)
}

func (c *Conn) Pop3Banner(b []byte) (int, error) {
	n, err := c.readPop3Response(b)
	rs := readState{
		response: b[0:n],
		err: err,
	}
	c.operations = append(c.operations, &rs)
	return n, err
}

func (c *Conn) readImapStatusResponse(res []byte) (int, error) {
	return c.readUntilRegex(res, imapStatusEndRegex)
}

func (c *Conn) ImapBanner(b []byte) (int, error) {
	n, err := c.readImapStatusResponse(b)
	rs := readState {
		response: b[0:n],
		err: err,
	}
	c.operations = append(c.operations, &rs)
	return n, err
}

func (c *Conn) SendHeartbleedProbe(b []byte) (int, error) {
	if !c.isTls {
		return 0, fmt.Errorf(
			"Must perform TLS handshake before sending Heartbleed probe to %s",
			c.RemoteAddr().String())
	}
	n, err := c.tlsConn.CheckHeartbleed(b)
	hl := c.tlsConn.HeartbleedLog()
	if err == ztls.HeartbleedError {
		err = nil
	}
	hs := heartbleedState{probe: hl, err: err}
	c.operations = append(c.operations, &hs)
	return n, err
}

func (c *Conn) States() []StateLog {
	states := make([]StateLog, 0, len(c.operations))
	for _, state := range c.operations {
		states = append(states, state.StateLog())
	}
	return states
}