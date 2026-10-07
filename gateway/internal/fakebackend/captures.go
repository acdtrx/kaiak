package fakebackend

import (
	"embed"
	"path"
)

// captures holds real backends' recorded answers (captures/README.md says which
// servers and versions).
//
//go:embed captures
var captures embed.FS

// Captured is the answer recorded from server (a directory of captures/, e.g. "vllm")
// as file name. It panics when there is no such recording: a test naming one is wrong.
func Captured(server, name string) []byte {
	data, err := captures.ReadFile(path.Join("captures", server, name))
	if err != nil {
		panic("fakebackend: " + err.Error())
	}
	return data
}
