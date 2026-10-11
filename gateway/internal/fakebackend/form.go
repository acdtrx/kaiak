package fakebackend

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
)

// uploadForm is an upload's form as the servers' routes read it — image edits and
// transcriptions: its text parts' values by name, and how many file parts each name
// holds.
type uploadForm struct {
	text  map[string][]string
	files map[string]int
}

// value is the form's text part name, and whether it has one.
func (f uploadForm) value(name string) (string, bool) {
	if v := f.text[name]; len(v) > 0 {
		return v[0], true
	}
	return "", false
}

// readUploadForm reads an upload's body as a multipart/form-data form under the
// boundary its Content-Type names: a body of another type reads as an empty form, as
// Starlette's does; ok is false for a form that does not parse. A part is a file part
// when its Content-Disposition names a filename, even an empty one, as Starlette reads
// it.
func readUploadForm(contentType string, body []byte) (form uploadForm, ok bool) {
	form = uploadForm{text: map[string][]string{}, files: map[string]int{}}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return form, true
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return form, true
		}
		if err != nil {
			return form, false
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return form, false
		}
		_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return form, false
		}
		name := disposition["name"]
		if _, file := disposition["filename"]; file {
			form.files[name]++
		} else {
			form.text[name] = append(form.text[name], string(data))
		}
	}
}
