package migrate

import (
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"strings"
)

const (
	tempPrefix, tempSuffix = ".migrate-", ".tmp"
	runLen, runAlphabet    = 26, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

func tempName(run string, seq int) string {
	return tempPrefix + run + "-" + strconv.Itoa(seq) + tempSuffix
}

func tempRun(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, tempPrefix)
	if ok {
		rest, ok = strings.CutSuffix(rest, tempSuffix)
	}
	run, seq, cut := strings.Cut(rest, "-")
	ok = ok && cut && len(run) == runLen && strings.Trim(run, runAlphabet) == "" && seq != "" && strings.Trim(seq, "0123456789") == ""
	return run, ok
}

var errNotImplemented = errors.New("not implemented")

type Publisher struct {
	run    string
	seq    int
	rename func(dirfd int, oldName, newName string) error
	at     func(step string) error
}

func NewPublisher() (*Publisher, error) {
	return &Publisher{run: rand.Text(), rename: noReplaceRename}, nil
}

func (p *Publisher) Publish(dir *Dir, leaf string, src io.ReaderAt, size int64, mode fs.FileMode) (Result, error) {
	return ResultFailed, errNotImplemented
}

func (p *Publisher) EnsureProjectRoot(pair *Pair) (*Dir, error) { return nil, errNotImplemented }

func (p *Publisher) OlderTemps(dir *Dir) ([]string, error) { return nil, errNotImplemented }
