package fdbx

func newMockDB(conn *MockConn) *mockDB { return &mockDB{MockConn: conn} }

type mockDB struct{ *MockConn }

func (db *mockDB) ClearAll() error                             { return db.FClearAll() }
func (db *mockDB) Set(ctype uint16, id, value []byte) error    { return db.FSet(ctype, id, value) }
func (db *mockDB) Get(ctype uint16, id []byte) ([]byte, error) { return db.FGet(ctype, id) }
func (db *mockDB) Del(ctype uint16, id []byte) error           { return db.FDel(ctype, id) }
func (db *mockDB) Save(m ...Model) error                       { return db.FSave(m...) }
func (db *mockDB) Load(m ...Model) error                       { return db.FLoad(m...) }
func (db *mockDB) Drop(m ...Model) error                       { return db.FDrop(m...) }
func (db *mockDB) Select(ctype uint16, fab Fabric, opts ...Option) ([]Model, error) {
	return db.FSelect(ctype, fab, opts...)
}
