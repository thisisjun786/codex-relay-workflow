package recall

import "errors"

type RwDb struct{}
type Stmt struct{}
type RunResult struct{ Changes, LastInsertRowid float64 }

func openDbReadOnly(path string) (*RwDb, error)             { return nil, errors.New("port stub") }
func openDbReadWrite(path string) (*RwDb, error)            { return nil, errors.New("port stub") }
func (d *RwDb) Prepare(query string) (*Stmt, error)         { return nil, errors.New("port stub") }
func (d *RwDb) Exec(query string) error                     { return errors.New("port stub") }
func (d *RwDb) Close() error                                { return nil }
func (s *Stmt) All(params ...any) ([]map[string]any, error) { return nil, errors.New("port stub") }
func (s *Stmt) Get(params ...any) (map[string]any, error)   { return nil, errors.New("port stub") }
func (s *Stmt) Run(params ...any) (RunResult, error)        { return RunResult{}, errors.New("port stub") }
