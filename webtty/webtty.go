package webtty

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"

	"github.com/pborman/ansi"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/sorenisanerd/gotty/utils"
)

// WebTTY bridges a PTY slave and its PTY master.
// To support text-based streams and side channel commands such as
// terminal resizing, WebTTY uses an original protocol.
type WebTTY struct {
	// PTY Master, which probably a connection to browser
	masterConn Master
	// PTY Slave
	slave Slave

	windowTitle  []byte
	permitWrite  bool
	columns      int
	rows         int
	reconnect    int // in seconds
	masterPrefs  []byte
	username     string
	auditEnabled bool
	decoder      Decoder

	bufferSize int
	writeMutex sync.Mutex
	feBuffer   []byte
	beBuffer   []byte
	logger     *zerolog.Logger
}

// New creates a new instance of WebTTY.
// masterConn is a connection to the PTY master,
// typically it's a websocket connection to a client.
// slave is a PTY slave such as a local command with a PTY.
func New(masterConn Master, slave Slave, options ...Option) (*WebTTY, error) {
	wt := &WebTTY{
		masterConn: masterConn,
		slave:      slave,

		permitWrite: false,
		columns:     0,
		rows:        0,

		bufferSize: 1024,
		decoder:    &NullCodec{},
		logger:     &log.Logger,
	}

	for _, option := range options {
		option(wt)
	}

	return wt, nil
}

// Run starts the main process of the WebTTY.
// This method blocks until the context is canceled.
// Note that the master and slave are left intact even
// after the context is canceled. Closing them is caller's
// responsibility.
// If the connection to one end gets closed, returns ErrSlaveClosed or ErrMasterClosed.
func (wt *WebTTY) Run(ctx context.Context) error {
	err := wt.sendInitializeMessage()
	if err != nil {
		return errors.Wrapf(err, "failed to send initializing message")
	}

	errs := make(chan error, 2)

	go func() {
		errs <- func() error {
			buffer := make([]byte, wt.bufferSize)
			for {
				//base64 length
				effectiveBufferSize := wt.bufferSize - 1
				//max raw data length
				maxChunkSize := int(effectiveBufferSize/4) * 3

				n, err := wt.slave.Read(buffer[:maxChunkSize])
				if err != nil {
					return ErrSlaveClosed
				}
				wt.auditLogs(buffer[:n], false)
				err = wt.handleSlaveReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	go func() {
		errs <- func() error {
			buffer := make([]byte, wt.bufferSize)
			for {
				n, err := wt.masterConn.Read(buffer)
				if err != nil {
					return ErrMasterClosed
				}

				err = wt.handleMasterReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
	}

	return err
}

func (wt *WebTTY) sendInitializeMessage() error {
	err := wt.masterWrite(append([]byte{SetWindowTitle}, wt.windowTitle...))
	if err != nil {
		return errors.Wrapf(err, "failed to send window title")
	}

	bufSizeMsg, _ := json.Marshal(wt.bufferSize)
	err = wt.masterWrite(append([]byte{SetBufferSize}, bufSizeMsg...))
	if err != nil {
		return errors.Wrapf(err, "failed to send buffer size")
	}

	if wt.reconnect > 0 {
		reconnect, _ := json.Marshal(wt.reconnect)
		err := wt.masterWrite(append([]byte{SetReconnect}, reconnect...))
		if err != nil {
			return errors.Wrapf(err, "failed to set reconnect")
		}
	}

	if wt.masterPrefs != nil {
		err := wt.masterWrite(append([]byte{SetPreferences}, wt.masterPrefs...))
		if err != nil {
			return errors.Wrapf(err, "failed to set preferences")
		}
	}

	return nil
}

func (wt *WebTTY) handleSlaveReadEvent(data []byte) error {
	safeMessage := base64.StdEncoding.EncodeToString(data)
	err := wt.masterWrite(append([]byte{Output}, []byte(safeMessage)...))
	if err != nil {
		return errors.Wrapf(err, "failed to send message to master")
	}

	return nil
}

func (wt *WebTTY) masterWrite(data []byte) error {
	wt.writeMutex.Lock()
	defer wt.writeMutex.Unlock()

	_, err := wt.masterConn.Write(data)
	if err != nil {
		return errors.Wrapf(err, "failed to write to master")
	}

	return nil
}

func (wt *WebTTY) handleMasterReadEvent(data []byte) error {
	if len(data) == 0 {
		return errors.New("unexpected zero length read from master")
	}

	switch data[0] {
	case Input:
		if !wt.permitWrite {
			return nil
		}

		if len(data) <= 1 {
			return nil
		}

		var decodedBuffer = make([]byte, len(data))
		n, err := wt.decoder.Decode(decodedBuffer, data[1:])
		if err != nil {
			return errors.Wrapf(err, "failed to decode received data")
		}

		wt.auditLogs(decodedBuffer[:n], true)
		_, err = wt.slave.Write(decodedBuffer[:n])
		if err != nil {
			return errors.Wrapf(err, "failed to write received data to slave")
		}

	case Ping:
		err := wt.masterWrite([]byte{Pong})
		if err != nil {
			return errors.Wrapf(err, "failed to return Pong message to master")
		}

	case SetEncoding:
		switch string(data[1:]) {
		case "base64":
			wt.decoder = base64.StdEncoding
		case "null":
			wt.decoder = NullCodec{}
		}

	case ResizeTerminal:
		if wt.columns != 0 && wt.rows != 0 {
			break
		}

		if len(data) <= 1 {
			return errors.New("received malformed remote command for terminal resize: empty payload")
		}

		var args argResizeTerminal
		err := json.Unmarshal(data[1:], &args)
		if err != nil {
			return errors.Wrapf(err, "received malformed data for terminal resize")
		}
		rows := wt.rows
		if rows == 0 {
			rows = int(args.Rows)
		}

		columns := wt.columns
		if columns == 0 {
			columns = int(args.Columns)
		}

		wt.slave.ResizeTerminal(columns, rows)
	default:
		return errors.Errorf("unknown message type `%c`", data[0])
	}

	return nil
}

type argResizeTerminal struct {
	Columns float64
	Rows    float64
}

func (wt *WebTTY) auditLogs(buffer []byte, fromFe bool) {
	if !wt.auditEnabled {
		return
	}
	buffer, _ = ansi.Strip(buffer)
	if fromFe {
		wt.feBuffer = append(wt.feBuffer, buffer...)
		if pos := bytes.LastIndexByte(wt.feBuffer, byte(13)); pos > -1 { // 13 = carriage return
			wt.printAuditLogs(wt.feBuffer[:pos], fromFe)
			wt.feBuffer = wt.feBuffer[pos+1:]
		}
	} else {
		wt.beBuffer = append(wt.beBuffer, buffer...)
		if pos := bytes.LastIndexByte(wt.beBuffer, byte(13)); pos > -1 { // 13 = carriage return
			wt.printAuditLogs(wt.beBuffer[:pos], fromFe)
			wt.beBuffer = wt.beBuffer[pos+1:]
		}
	}
}

func (wt *WebTTY) printAuditLogs(logs []byte, fe bool) {
	stream := "browser"
	if !fe {
		stream = "backend"
	}

	logger := wt.logger.Info().Str("log-type", "audit").Str("stream", stream)
	if wt.username != "" {
		logger.Str("username", wt.username)
	}
	logger.Msg(utils.RemoveNonGraphicChar(string(logs)))
}
