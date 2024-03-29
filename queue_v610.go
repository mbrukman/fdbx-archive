package fdbx

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// PunchSize - размер ожидания в случае отсутствия задач
var PunchSize = time.Minute

func newV610queue(c *v610Conn, qtype uint16, f Fabric) (q *v610queue, err error) {
	q = &v610queue{
		id: qtype,
		cn: c,
		mf: f,
	}

	if q.kr.Begin, err = c.Key(qtype, []byte{0x00}); err != nil {
		return
	}

	if q.kr.End, err = c.Key(qtype, []byte{0xFF}); err != nil {
		return
	}

	if q.wk, err = q.cn.Key(qtype, []byte{0xFF, 0xFF}); err != nil {
		return
	}

	return q, nil
}

type v610queue struct {
	id uint16
	mf Fabric
	cn *v610Conn
	wk fdb.Key
	kr fdb.KeyRange
}

func (q *v610queue) Ack(db DB, m Model) error {
	if db == nil {
		return ErrNullDB.WithStack()
	}

	if m == nil {
		return ErrNullModel.WithStack()
	}

	return db.Del(q.id, append([]byte{0xFF}, m.ID()...))
}

func (q *v610queue) Pub(db DB, m Model, t time.Time) (err error) {
	if db == nil {
		return ErrNullDB.WithStack()
	}

	if m == nil {
		return ErrNullModel.WithStack()
	}

	if t.IsZero() {
		t = time.Now()
	}

	mid := m.ID()
	key := make([]byte, 8+len(mid))
	binary.BigEndian.PutUint64(key[:8], uint64(t.UnixNano()))

	if n := copy(key[8:], mid); n != len(mid) {
		return ErrMemFail.WithStack()
	}

	if err = db.Set(q.id, key, nil); err != nil {
		return
	}

	// update watch
	return db.Set(q.id, []byte{0xFF, 0xFF}, key[:8])
}

func (q *v610queue) Sub(ctx context.Context) (<-chan Model, <-chan error) {
	modc := make(chan Model)
	errc := make(chan error, 1)

	go func() {
		var m Model
		var err error

		defer close(errc)
		defer close(modc)
		defer func() {
			if rec := recover(); rec != nil {

				if err, ok := rec.(error); ok {
					errc <- ErrQueuePanic.WithReason(err)
				} else {
					errc <- ErrQueuePanic.WithReason(fmt.Errorf("%+v", rec))
				}
			}
		}()

		for {

			if m, err = q.SubOne(ctx); err != nil {
				errc <- err
				return
			}

			select {
			case modc <- m:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
	}()

	return modc, errc
}

func (q *v610queue) SubOne(ctx context.Context) (_ Model, err error) {
	var list []Model

	if list, err = q.SubList(ctx, 1); err != nil {
		return
	}
	return list[0], nil
}

func (q *v610queue) nextTaskDistance() (d time.Duration, err error) {
	d = PunchSize
	_, err = q.cn.fdb.ReadTransact(func(tx fdb.ReadTransaction) (_ interface{}, e error) {
		rows := tx.GetRange(q.kr, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll, Limit: 1}).GetSliceOrPanic()
		if len(rows) > 0 {
			if wait := time.Unix(0, int64(binary.BigEndian.Uint64(rows[0].Key[4:12]))).Sub(time.Now()); wait > 0 {
				d = wait + time.Millisecond
			}
		}
		return nil, nil
	})
	return d, err
}

func (q *v610queue) SubList(ctx context.Context, limit int) (list []Model, err error) {
	var ids [][]byte
	var wait fdb.FutureNil
	var punch time.Duration

	for len(list) < limit {

		if wait != nil {
			if punch, err = q.nextTaskDistance(); err != nil {
				return
			}

			wc := make(chan struct{}, 1)
			go func() { defer close(wc); wait.BlockUntilReady(); wc <- struct{}{} }()

			func() {
				wctx, cancel := context.WithTimeout(ctx, punch)
				defer cancel()

				select {
				case <-wc:
				case <-wctx.Done():
					wait.Cancel()
					return
				}
			}()
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// select ids in self tx
		_, err = q.cn.fdb.Transact(func(tx fdb.Transaction) (_ interface{}, e error) {
			ids = make([][]byte, 0, limit)

			var kr fdb.KeyRange
			var rows []fdb.KeyValue

			now := make([]byte, 8)
			binary.BigEndian.PutUint64(now, uint64(time.Now().UnixNano()))

			if kr.Begin, e = q.cn.Key(q.id, []byte{0x00}); e != nil {
				return
			}

			if kr.End, e = q.cn.Key(q.id, now); e != nil {
				return
			}

			lim := limit - len(list)

			if lim < 1 {

				return nil, nil
			}

			// must lock this range from parallel reads
			if e = tx.AddWriteConflictRange(kr); e != nil {
				return
			}

			opts := fdb.RangeOptions{Mode: fdb.StreamingModeWantAll, Limit: lim}

			if rows = tx.GetRange(kr, opts).GetSliceOrPanic(); len(rows) == 0 {
				wait = tx.Watch(q.wk)
				return nil, nil
			}

			var ack fdb.Key

			for i := range rows {
				mid := rows[i].Key[12:]
				ids = append(ids, mid)

				if ack, e = q.cn.Key(q.id, append([]byte{0xFF}, mid...)); e != nil {
					return
				}

				// move to lost
				tx.Set(ack, nil)
				tx.Clear(rows[i].Key)
			}

			return nil, nil
		})
		if err != nil {
			return
		}

		if len(ids) == 0 {
			continue
		}

		models := make([]Model, len(ids))
		for i := range ids {
			if models[i], err = q.mf(ids[i]); err != nil {
				return
			}
		}

		if err = q.cn.Tx(func(db DB) error { return db.Load(models...) }); err != nil {
			return
		}

		list = append(list, models...)
	}

	return list, nil
}

func (q *v610queue) GetLost(limit int) ([]Model, error) { return nil, nil }
func (q *v610queue) Settings() (uint16, Fabric)         { return q.id, q.mf }
