package web

import (
	"io/fs"
	"testing/fstest"
)

type fstestMap map[string]string

func (m fstestMap) fs() fs.FS {
	out := fstest.MapFS{}
	for k, v := range m {
		out[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return out
}
