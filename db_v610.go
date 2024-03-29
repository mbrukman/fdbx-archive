package fdbx

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/google/uuid"
)

// ChunkType is collection number for storing blob chunks. Default uint16 max value
var ChunkType = uint16(0xFFFF)

// ChunkSize is max chunk length. Default 100 Kb - fdb value limit
var ChunkSize = 100000

// MaxChunkSize is max possible chunk size.
const MaxChunkSize = 100000

// GZipSize is a value len more then GZipSize cause gzip processing
var GZipSize = 860

const (
	flagGZip  = uint8(1 << 6)
	flagChunk = uint8(1 << 7)
)

func newV610db(c *v610Conn, tx fdb.Transaction) (db *v610db, err error) {
	db = &v610db{conn: c, tx: tx}

	return db, nil
}

type v610db struct {
	conn *v610Conn
	tx   fdb.Transaction
}

func (db *v610db) ClearAll() error {
	dbtype := db.conn.DB()

	// all plain data
	begin := make(fdb.Key, 2)
	binary.BigEndian.PutUint16(begin[0:2], dbtype)
	end := make(fdb.Key, 5)
	binary.BigEndian.PutUint16(end[0:2], dbtype)
	binary.BigEndian.PutUint16(end[2:4], 0xFFFF)
	end[4] = 0xFF
	db.tx.ClearRange(fdb.KeyRange{Begin: begin, End: end})

	return nil
}

func (db *v610db) Get(ctype uint16, id []byte) (_ []byte, err error) {
	var key fdb.Key

	if key, err = db.conn.Key(ctype, id); err != nil {
		return
	}

	return db.tx.Get(key).Get()
}

func (db *v610db) Set(ctype uint16, id, value []byte) (err error) {
	var key fdb.Key

	if key, err = db.conn.Key(ctype, id); err != nil {
		return
	}

	db.tx.Set(key, value)

	return nil
}

func (db *v610db) Del(ctype uint16, id []byte) (err error) {
	var key fdb.Key

	if key, err = db.conn.Key(ctype, id); err != nil {
		return
	}

	db.tx.Clear(key)

	return nil
}

func (db *v610db) Save(models ...Model) (err error) {
	for i := range models {
		if err = db.save(models[i]); err != nil {
			return
		}
	}

	return nil
}

func (db *v610db) Load(models ...Model) (err error) {
	var key fdb.Key

	// query all futures to leverage wait time
	futures := make([]fdb.FutureByteSlice, 0, len(models))

	for i := range models {
		if key, err = db.conn.MKey(models[i]); err != nil {
			return
		}

		futures = append(futures, db.tx.Get(key))
	}

	for i := range futures {
		if err = db.load(models[i], futures[i]); err != nil {
			return
		}
	}

	return nil
}

func (db *v610db) Drop(models ...Model) (err error) {
	keys := make(map[int]fdb.Key, len(models))
	futures := make([]fdb.FutureByteSlice, 0, len(models))

	for i := range models {
		if keys[i], err = db.conn.MKey(models[i]); err != nil {
			return
		}

		futures = append(futures, db.tx.Get(keys[i]))
	}

	for i := range futures {
		if err = db.drop(models[i], futures[i]); err != nil {
			return
		}

		db.tx.Clear(keys[i])
	}

	return nil
}

func (db *v610db) Select(ctype uint16, fab Fabric, opts ...Option) (list []Model, err error) {
	var m Model
	var kr fdb.KeyRange
	var mid, value []byte

	o := new(options)

	for i := range opts {
		if err = opts[i](o); err != nil {
			return
		}
	}

	ro := fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}

	if o.limit > 0 {
		ro.Limit = o.limit
	}

	gte := []byte{0x00}
	if o.gte != nil {
		gte = o.gte
	}

	if kr.Begin, err = db.conn.Key(ctype, gte); err != nil {
		return
	}

	lt := []byte{0xFF}
	if o.lt != nil {
		lt = o.lt
	}

	if kr.End, err = db.conn.Key(ctype, lt); err != nil {
		return
	}

	rows := db.tx.GetRange(kr, ro).GetSliceOrPanic()
	list = make([]Model, 0, len(rows))
	for i := range rows {
		if o.idlen > 0 {
			in := len(rows[i].Key) - o.idlen
			mid = rows[i].Key[in:]
		} else if o.pflen > 0 {
			mid = rows[i].Key[o.pflen:]
		}

		if _, value, err = db.unpack(rows[i].Value); err != nil {
			return
		}

		add := true
		if o.filter != nil {
			if add, err = o.filter(value); err != nil {
				return
			}
		}

		if !add {
			continue
		}

		if m, err = fab(mid); err != nil {
			return
		}

		if err = m.Load(value); err != nil {
			return
		}

		list = append(list, m)

	}
	return list, nil
}

// *********** private ***********

func (db *v610db) pack(buffer []byte) (_ []byte, err error) {
	var flags uint8

	// so long, try to reduce
	if len(buffer) > GZipSize {
		if buffer, err = db.gzipValue(&flags, buffer); err != nil {
			return
		}
	}

	// sooooooo long, we must split and save as blob
	if len(buffer) > ChunkSize {
		if buffer, err = db.saveBlob(&flags, buffer); err != nil {
			return
		}
	}

	return append([]byte{flags}, buffer...), nil
}

func (db *v610db) unpack(value []byte) (blobID, buffer []byte, err error) {
	flags := value[0]
	buffer = value[1:]

	// blob data
	if flags&flagChunk > 0 {
		blobID = buffer

		if buffer, err = db.loadBlob(buffer); err != nil {
			return
		}
	}

	// gzip data
	if flags&flagGZip > 0 {
		if buffer, err = db.gunzipValue(buffer); err != nil {
			return
		}
	}

	return blobID, buffer, nil
}

func (db *v610db) drop(m Model, fb fdb.FutureByteSlice) (err error) {
	var idx fdb.Key
	var value, blobID []byte

	if value, err = fb.Get(); err != nil {
		return
	}

	if len(value) == 0 {
		return nil
	}

	if blobID, value, err = db.unpack(value); err != nil {
		return
	}
	if blobID != nil {
		if err = db.dropBlob(blobID); err != nil {
			return
		}
	}

	// plain buffer needed for index calc
	for _, index := range db.conn.Indexes(m.Type()) {
		if idx, err = index(value); err != nil {
			return
		}

		db.tx.Clear(idx)
	}

	return nil
}

func (db *v610db) save(m Model) (err error) {

	var value []byte
	var key, idx fdb.Key

	// basic model key
	if key, err = db.conn.MKey(m); err != nil {
		return
	}

	// type index list
	indexes := db.conn.Indexes(m.Type())

	// old data dump for index invalidate
	if dump := m.Dump(); len(dump) > 0 {
		for _, index := range indexes {
			if idx, err = index(dump); err != nil {
				return
			}

			db.tx.Clear(idx)
		}
	}

	// plain object buffer
	if value, err = m.Pack(); err != nil {
		return
	}

	// new index keys
	for _, index := range indexes {
		if idx, err = index(value); err != nil {
			return
		}

		db.tx.Set(idx, nil)
	}

	if value, err = db.pack(value); err != nil {
		return
	}

	db.tx.Set(key, value)
	return nil
}

func (db *v610db) load(m Model, fb fdb.FutureByteSlice) (err error) {
	var value []byte

	if value, err = fb.Get(); err != nil {
		return
	}

	if len(value) == 0 {
		// it's model responsibility for loading control
		return nil
	}

	if _, value, err = db.unpack(value); err != nil {
		return
	}

	// plain buffer
	return m.Load(value)
}

func (db *v610db) gzipValue(flags *uint8, value []byte) ([]byte, error) {
	*flags |= flagGZip

	// TODO: sync.Pool
	buf := new(bytes.Buffer)

	if err := db.gzip(buf, bytes.NewReader(value)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (db *v610db) gunzipValue(value []byte) ([]byte, error) {
	// TODO: sync.Pool
	buf := new(bytes.Buffer)

	if err := db.gunzip(buf, bytes.NewReader(value)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (db *v610db) gzip(w io.Writer, r io.Reader) (err error) {
	gw := gzip.NewWriter(w)

	defer func() {
		e := gw.Close()
		if err == nil {
			err = e
		}
	}()

	if _, err = io.Copy(gw, r); err != nil {
		return ErrMemFail.WithReason(err)
	}

	return nil
}

func (db *v610db) gunzip(w io.Writer, r io.Reader) (err error) {
	var gr *gzip.Reader

	if gr, err = gzip.NewReader(r); err != nil {
		return ErrInvalidGZ.WithReason(err)
	}

	defer func() {
		e := gr.Close()
		if err == nil {
			err = e
		}
	}()

	if _, err = io.Copy(w, gr); err != nil {
		return ErrMemFail.WithReason(err)
	}

	return nil
}

func (db *v610db) saveBlob(flags *uint8, blob []byte) (value []byte, err error) {
	var i uint16
	var last bool
	var part, key []byte
	var index [2]byte

	*flags |= flagChunk
	blobID := uuid.New()

	if key, err = db.conn.Key(ChunkType, blobID[:]); err != nil {
		return
	}

	// TODO: only up to 10M (transaction size)
	// split into multiple goroutines for speed
	for !last {
		// check tail
		if len(blob) <= ChunkSize {
			last = true
			part = blob
		} else {
			part = blob[:ChunkSize]
			blob = blob[ChunkSize:]
		}

		// save part
		binary.BigEndian.PutUint16(index[:], i)
		db.tx.Set(fdb.Key(append(key, index[0], index[1])), part)
		i++
	}

	return blobID[:], nil
}

func (db *v610db) loadBlob(value []byte) (blob []byte, err error) {
	var key fdb.Key
	var kv fdb.KeyValue

	if key, err = db.conn.Key(ChunkType, value); err != nil {
		return
	}

	kr := fdb.KeyRange{Begin: key, End: fdb.Key(append(key, 255))}
	res := db.tx.GetRange(kr, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()

	for res.Advance() {
		if kv, err = res.Get(); err != nil {
			return
		}
		blob = append(blob, kv.Value...)
	}
	return blob, nil
}

func (db *v610db) dropBlob(value []byte) (err error) {
	var key fdb.Key

	if key, err = db.conn.Key(ChunkType, value); err != nil {
		return
	}

	db.tx.ClearRange(fdb.KeyRange{Begin: key, End: fdb.Key(append(key, 255))})
	return nil
}
