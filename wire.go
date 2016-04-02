package DHTCrawl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/zeebo/bencode"
	"math"
	"net"
	"time"
)

const (
	BtProtocol   = "BitTorrent protocol"
	BtExtendedID = byte(0)
	BtMessageID  = byte(20)

	PieceSize       = 1 << 14
	MaxMetadataSize = 1 << 20

	EventError = iota - 1
	EventHandshake
	EventExtended
	EventPiece
	EventDone
)

var (
	//[5] = 1 as extension, [7] = 1 as dht
	BtReserved = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x01}
)

type (
	DataHandler func([]byte)

	File struct {
		Path string `bencode:"path"`
		Size int64  `bencode:"length"`
	}

	MetadataResult struct {
		Error  error
		Hash   Hash
		Name   string      `bencode:"name"`
		Size   int64       `bencode:"length"`
		Files  []*File     `bencode:"files"`
		Pieces interface{} `bencode:"pieces"`
	}

	Event struct {
		Type   int
		Reason string
		Result *MetadataResult
	}

	Processor struct {
		Hash Hash

		Data [][]byte
		Size int

		Handler     DataHandler
		HandlerSize int

		utmetadata   int
		metadata     [][]byte
		metadataSize int
		pieceLength  int
		recvedPiece  int

		output chan []byte
		event  chan *Event
	}

	Wire struct {
		Addr      *net.TCPAddr
		Conn      net.Conn
		Processor *Processor
		Result    chan *MetadataResult
	}
)

func NewError(reason string) *MetadataResult {
	return &MetadataResult{Error: errors.New(reason)}
}

func NewErrorEvent(reason string) *Event {
	return &Event{Type: EventError, Reason: reason}
}

func NewWire() *Wire {
	wire := new(Wire)
	wire.Result = make(chan *MetadataResult)
	wire.Processor = &Processor{
		output: make(chan []byte),
		Data:   [][]byte{},
	}
	return wire
}

func (w *Wire) Download(hash Hash, addr *net.TCPAddr) {
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second*2)
	if err != nil {
		fmt.Printf("%s , %s\n", err.Error(), addr.String())
		w.Result <- NewError(err.Error())
		return
	}
	w.Conn = conn
	go w.Pipe()
	w.Processor.Start(hash)
	buf := make([]byte, 512)
	for {
		n, err := w.Conn.Read(buf)
		if err != nil {
			w.Result <- NewError(err.Error())
			break
		}
		w.Processor.Write(buf[:n])
	}
}

func (w *Wire) Pipe() {
	for {
		select {
		case data := <-w.Processor.output:
			w.Conn.Write(data)
		case event, ok := <-w.Processor.event:
			if !ok {
				w.Conn.Close()
				break
			}
			switch event.Type {
			case EventError:
				fmt.Println(event.Reason)
				w.Result <- NewError(event.Reason)
			case EventDone:
				w.Result <- event.Result
			case EventHandshake:
				fmt.Println("Handshake success")
			case EventExtended:
				fmt.Println("Extended success")
			case EventPiece:
				fmt.Println("piece success")
			}
		}
	}
}

func (p *Processor) Write(data []byte) (int, error) {
	p.Size += len(data)
	p.Data = append(p.Data, data)
	for p.Size >= p.HandlerSize {
		buf := bytes.Join(p.Data, []byte{})
		p.Size -= p.HandlerSize
		if p.Size == 0 {
			p.Data = [][]byte{}
		} else {
			p.Data = [][]byte{buf[p.HandlerSize:]}
		}
		p.Handler(buf[:p.HandlerSize])
	}
	return len(data), nil
}

func (p *Processor) Start(hash Hash) {
	p.Hash = hash
	p.output <- p.packetHandshakeData()
	p.handleHandshake()
}

func (p *Processor) process(size int, handler DataHandler) {
	p.HandlerSize = size
	p.Handler = handler
}

func (p *Processor) End(reason string) {
	p.event <- NewErrorEvent(reason)
	close(p.event)
}

func (p *Processor) handleHandshake() {
	p.process(1, func(data []byte) {
		length := int(data[0])
		p.process(length+48, func(data []byte) {
			protocol := data[:length]
			if string(protocol) != BtProtocol {
				p.End("this is not BitTorrent protocol")
				return
			}
			reserved := data[length:]
			if reserved[5]&0x10 == 0 {
				p.End("peer reject")
				return
			}
			p.event <- &Event{Type: EventHandshake}
			p.process(4, p.handleHead)
			p.output <- p.packetExtendedData()
		})
	})
}

func (p *Processor) handleHead(data []byte) {
	var length uint32
	binary.Read(bytes.NewReader(data), binary.BigEndian, &length)
	if int(length) > 0 {
		p.process(int(length), p.handleBody)
	}
}

func (p *Processor) handleBody(data []byte) {
	p.process(4, p.handleHead)
	if data[0] == BtExtendedID {
		p.handleExtended(data[1], data[2:])
	}
}

func (p *Processor) handleExtended(ext byte, data []byte) {
	if ext == byte(0) {
		val := make(map[string]interface{})
		err := bencode.DecodeBytes(data, &val)
		if err != nil {
			p.End(fmt.Sprintf("decode extended meta info error %s", err.Error()))
			return
		}
		p.handleExtHandshake(val)
	} else {
		p.handlePiece(data)
	}
}

func (p *Processor) handleExtHandshake(ext map[string]interface{}) {
	p.event <- &Event{Type: EventExtended}
	if size, ok := ext["metadata_size"].(int64); ok {
		if m, ok := ext["m"].(map[string]interface{}); ok {
			if meta, ok := m["ut_metadata"].(int64); ok {
				p.utmetadata = int(meta)
				p.pieceLength = int(math.Ceil(float64(size) / float64(PieceSize)))
				p.metadata = make([][]byte, p.pieceLength)

				if p.utmetadata == 0 || size == 0 || size > MaxMetadataSize {
					p.End(fmt.Sprintf("extended invalid metadata_size:%d, ut_metadata:%d", size, p.utmetadata))
					return
				}
				for i := 0; i < p.pieceLength; i++ {
					p.output <- p.packetPieceRequestData(i)
				}
			}
		}
	}
}

func (p *Processor) handlePiece(data []byte) {
	p.event <- &Event{Type: EventPiece}
	i := bytes.Index(data, []byte{101, 101})
	if i == -1 {
		p.End("invalid piece info dict")
		return
	}
	info := make(map[string]interface{})
	err := bencode.DecodeBytes(data[0:i+2], &info)
	if err != nil {
		p.End(fmt.Sprintf("decode piece dict error, %s", err.Error()))
		return
	}
	piece := data[i+2:]

	if t, ok := info["msg_type"].(int64); !ok || t != int64(1) {
		p.End(fmt.Sprintf("invalid msg_type: %d", t))
		return
	}

	n, ok := info["piece"].(int64)
	if !ok {
		p.End("invalid piece")
		return
	}

	p.metadata[int(n)] = piece
	p.recvedPiece++
	if p.recvedPiece == p.pieceLength {
		p.handleDone()
	}
}

func (p *Processor) handleDone() {
	data := bytes.Join(p.metadata, []byte{})
	result := new(MetadataResult)
	decoder := bencode.NewDecoder(bytes.NewReader(data))
	err := decoder.Decode(&result)
	if err != nil {
		p.End(fmt.Sprintf("Decode metadata error %s", err.Error()))
		return
	}
	result.Hash = p.Hash
	p.event <- &Event{Type: EventDone, Result: result}
	close(p.event)
}

func (p *Processor) packetHandshakeData() []byte {
	data := bytes.NewBuffer([]byte{})
	data.WriteByte(byte(0x13))
	data.WriteString(BtProtocol)
	data.Write(BtReserved)
	data.WriteString(string(p.Hash))
	data.Write([]byte(NewNodeID()))
	return data.Bytes()
}

func (p *Processor) packetExtendedData() []byte {
	body := bytes.NewBuffer([]byte{})
	body.WriteByte(BtMessageID)
	body.WriteByte(BtExtendedID)

	meta, _ := bencode.EncodeBytes(map[string]interface{}{"m": map[string]interface{}{"ut_metadata": 1}})
	body.Write(meta)

	data := bytes.NewBuffer([]byte{})
	binary.Write(data, binary.BigEndian, uint32(body.Len()))
	data.Write(body.Bytes())

	return data.Bytes()
}

func (p *Processor) packetPieceRequestData(i int) []byte {
	body := bytes.NewBuffer([]byte{})
	body.WriteByte(BtMessageID)
	body.WriteByte(byte(p.utmetadata))

	meta, _ := bencode.EncodeBytes(map[string]interface{}{"msg_type": 0, "piece": p})
	body.Write(meta)

	data := bytes.NewBuffer([]byte{})
	binary.Write(data, binary.BigEndian, uint32(body.Len()))
	data.Write(body.Bytes())

	return data.Bytes()
}
