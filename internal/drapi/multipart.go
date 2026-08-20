// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// FormField is a scalar multipart field accompanying an upload.
type FormField struct {
	Name  string
	Value string
}

// FormFile is the file part of an upload. Open is called once per attempt, so a retry
// replays the body from the start rather than resuming a drained reader.
type FormFile struct {
	// FieldName is the form field; the Files API always wants "file".
	FieldName string
	// Filename is the name to store, and it may contain "/" to place the object in a
	// directory. Verified: ".dr-sync/packs/p_x.tar.xz" round-trips byte-identically.
	Filename string
	Size     int64
	Open     func() (io.ReadCloser, error)
}

// buildMultipart assembles a streaming multipart/form-data body with an exact length.
//
// mime/multipart would need the whole body buffered or a pipe to produce a Content-Length,
// and an unknown length means chunked transfer encoding, which gateways are entitled to
// reject on a 256 MiB upload. Laying the envelope out by hand keeps the body streaming and
// the length known.
func buildMultipart(fields []FormField, file FormFile) (func() (io.ReadCloser, error), int64, string, error) {
	boundary, err := randomBoundary()
	if err != nil {
		return nil, 0, "", err
	}

	var prologue strings.Builder

	// Quotes are written literally around an already-escaped value. %q would apply Go's own
	// quoting on top, double-escaping every backslash and mangling non-ASCII names into
	// \u sequences, which is not what RFC 7578 asks for.
	for _, field := range fields {
		fmt.Fprintf(&prologue, "--%s\r\n", boundary)
		fmt.Fprintf(&prologue, "Content-Disposition: form-data; name=\"%s\"\r\n\r\n", escapeFormValue(field.Name))
		fmt.Fprintf(&prologue, "%s\r\n", field.Value)
	}

	fmt.Fprintf(&prologue, "--%s\r\n", boundary)
	fmt.Fprintf(&prologue, "Content-Disposition: form-data; name=\"%s\"; filename=\"%s\"\r\n",
		escapeFormValue(file.FieldName), escapeFormValue(file.Filename))
	prologue.WriteString("Content-Type: application/octet-stream\r\n\r\n")

	epilogue := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	head := prologue.String()
	length := int64(len(head)) + file.Size + int64(len(epilogue))

	body := func() (io.ReadCloser, error) {
		content, err := file.Open()
		if err != nil {
			return nil, err
		}

		reader := io.MultiReader(
			strings.NewReader(head),
			io.LimitReader(content, file.Size),
			strings.NewReader(epilogue),
		)

		return &multipartBody{Reader: reader, content: content}, nil
	}

	return body, length, "multipart/form-data; boundary=" + boundary, nil
}

// multipartBody closes the underlying file when the request body is closed.
type multipartBody struct {
	io.Reader
	content io.Closer
}

func (b *multipartBody) Close() error {
	return b.content.Close()
}

// randomBoundary returns a boundary that will not occur in the payload.
func randomBoundary() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating multipart boundary: %w", err)
	}

	return "dr-sync-" + hex.EncodeToString(buf), nil
}

// escapeFormValue escapes a header parameter per RFC 7578: backslash and quote are escaped,
// and newlines are dropped so a crafted filename cannot inject headers.
func escapeFormValue(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", `"`, `\"`, "\r", "", "\n", "")

	return replacer.Replace(value)
}
