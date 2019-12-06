package fdbx

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Supported FoundationDB client versions
const (
	ConnVersion610  = 610
	ConnVersionMock = 0xFFFF
)

const (
	// MaxChunkSize is max possible chunk size.
	MaxChunkSize = 100000
)

var (
	// ChunkType is collection number for storing blob chunks. Default uint16 max value
	ChunkType = uint16(0xFFFF)

	// ChunkSize is max chunk length. Default 100 Kb - fdb value limit
	ChunkSize = 100000

	// GZipSize is a value len more then GZipSize cause gzip processing
	GZipSize = 860
)

// TODO: agg funcs

// TxHandler -
type TxHandler func(DB) error

// RecordIndexes - calc index keys from model buffer
type RecordIndexes func(buffer []byte) (map[uint16][]byte, error)

// Fabric - model fabric func
type Fabric func(id []byte) (Record, error)

// Predicat - for query filtering, especially for seq scan queries
type Predicat func(buf []byte) (bool, error)

// Option -
type Option func(*options) error

// Conn - database connection (as stored database index)
type Conn interface {
	DB() uint16
	Key(typeID uint16, id []byte) (fdb.Key, error)
	MKey(Record) (fdb.Key, error)

	RecordIndexes(recordType uint16) RecordIndexes
	RegisterIndexFabric(recordType uint16, index RecordIndexes)

	ClearDB() error
	Tx(TxHandler) error

	Queue(qtype uint16, f Fabric) (Queue, error)

	Cursor(qtype uint16, f Fabric, start []byte, size uint32) (Cursor, error)
	LoadCursor(id uuid.UUID, size uint32) (Cursor, error)
}

// Queue -
type Queue interface {
	Ack(DB, Record) error

	Pub(DB, Record, time.Time) error

	Sub(ctx context.Context) (<-chan Record, <-chan error)
	SubOne(ctx context.Context) (Record, error)
	SubList(ctx context.Context, limit int) ([]Record, error)

	GetLost(limit int) ([]Record, error)

	Settings() (uint16, Fabric)
}

// DB - database object that holds connection for transaction handler
type DB interface {
	Set(typeID uint16, id, value []byte) error
	Get(typeID uint16, id []byte) ([]byte, error)
	Del(typeID uint16, id []byte) error

	Save(...Record) error
	Load(...Record) error
	Drop(...Record) error

	Select(indexID uint16, fab Fabric, opts ...Option) ([]Record, error)
}

// Record - database record object (user model, collection item)
type Record interface {
	// object identifier in any format
	FdbxID() []byte
	// type identifier (collection id)
	FdbxType() uint16
	// make new buffer from object fields
	FdbxMarshal() ([]byte, error)
	// fill object fields from buffer
	FdbxUnmarshal([]byte) error
}

// Cursor - helper for long seq scan queries or pagination
type Cursor interface {
	// cursor is saved to the database to eliminate transaction time limitation
	Record

	// if true, there are no records Next from cursor, but you can use Prev
	Empty() bool

	// mark cursor as empty and drop it from database
	Close() error

	// next or prev `page` records from collection or index
	Next(db DB, skip uint8) ([]Record, error)
	Prev(db DB, skip uint8) ([]Record, error)

	// select all records from current position to the end of collection
	Select() (<-chan Record, <-chan error)

	// current settings
	Settings() (uint16, Fabric)
}

// NewConn - makes new connection with specified client version
func NewConn(db, version uint16) (Conn, error) {
	// default 6.1.х
	if version == 0 {
		version = ConnVersion610
	}

	switch version {
	case ConnVersion610:
		return newV610Conn(db)
	case ConnVersionMock:
		return newMockConn(db)
	}

	return nil, ErrUnknownVersion
}
