// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// tarMedia is the body that means a tree rather than one file, which is
// what separates the two writes on the files collection (design 008).
const tarMedia = "application/x-tar"

// FileEntry is one file or directory as the API renders it. The mode is
// octal text: a JSON number for a permission set reads as decimal and is
// misread.
type FileEntry struct {
	// Name is the entry's base name, and Path its path in the workspace.
	Name string `json:"name"`
	Path string `json:"path"`
	// Size is the file's length in bytes.
	Size int64 `json:"size"`
	// Mode is the permission set in octal, such as 0644.
	Mode string `json:"mode"`
	// ModTime is when the entry last changed.
	ModTime time.Time `json:"modTime"`
	// IsDir says the entry is a directory.
	IsDir bool `json:"isDir"`
}

// filesPath is the file routes of one sandbox.
func filesPath(ref, suffix string) string {
	return KindSandbox.item(ref) + "/files" + suffix
}

// FileList reads a directory's immediate entries, sorted by name and whole:
// a workspace directory large enough to need a cursor is an archive.
func (c *Client) FileList(ctx context.Context, ref, path string) ([]FileEntry, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, filesPath(ref, "/list"), url.Values{"path": []string{path}}, nil, "")
	if err != nil {
		return nil, nil, err
	}
	var answer struct {
		Items []FileEntry `json:"items"`
	}
	if err = json.Unmarshal(raw, &answer); err != nil {
		return nil, nil, fmt.Errorf("the listing answer holds no entries: %w", err)
	}
	return answer.Items, raw, nil
}

// FileStat describes one entry.
func (c *Client) FileStat(ctx context.Context, ref, path string) (FileEntry, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, filesPath(ref, "/stat"), url.Values{"path": []string{path}}, nil, "")
	return decodeInto[FileEntry](raw, err)
}

// FileGet opens one file's body. The caller closes it.
func (c *Client) FileGet(ctx context.Context, ref, path string) (io.ReadCloser, error) {
	body, _, err := c.stream(ctx, http.MethodGet, filesPath(ref, "/content"), url.Values{"path": []string{path}}, nil, "")
	return body, err
}

// FilePut writes one file of any content type. A mode of zero leaves the
// server's default, which design 008 fixes at 0644.
func (c *Client) FilePut(ctx context.Context, ref, path string, mode string, body io.Reader) error {
	q := url.Values{"path": []string{path}}
	if mode != "" {
		q.Set("mode", mode)
	}
	return c.write(ctx, http.MethodPut, filesPath(ref, ""), q, body, "application/octet-stream")
}

// FileRemove deletes a file or a directory tree, and answers the same when
// it is already gone.
func (c *Client) FileRemove(ctx context.Context, ref, path string) error {
	return c.write(ctx, http.MethodDelete, filesPath(ref, ""), url.Values{"path": []string{path}}, nil, "")
}

// FileMkdir makes a directory and the missing parents.
func (c *Client) FileMkdir(ctx context.Context, ref, path string) error {
	body, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		return err
	}
	_, err = c.send(ctx, http.MethodPost, filesPath(ref, "/mkdir"), nil, body, "application/json")
	return err
}

// FileMove renames one path onto another, both inside the workspace.
func (c *Client) FileMove(ctx context.Context, ref, from, to string) error {
	body, err := json.Marshal(map[string]string{"from": from, "to": to})
	if err != nil {
		return err
	}
	_, err = c.send(ctx, http.MethodPost, filesPath(ref, "/move"), nil, body, "application/json")
	return err
}

// TarStream is an archive as the API answers it. Err is read once the body
// has been drained: design 008 says a failure after the first byte can no
// longer change the status, and the trailer carries its code instead.
type TarStream struct {
	io.ReadCloser
	trailer http.Header
}

// Err is the failure the trailer reported, or nil. It is meaningful only
// after the body has been read to its end.
func (t *TarStream) Err() error {
	if code := t.trailer.Get(errorTrailer); code != "" {
		return &StreamError{Code: code}
	}
	return nil
}

// ExportTar streams the named paths as one archive. The caller closes it.
func (c *Client) ExportTar(ctx context.Context, ref string, paths []string) (*TarStream, error) {
	q := url.Values{}
	for _, p := range paths {
		q.Add("path", p)
	}
	body, trailer, err := c.stream(ctx, http.MethodGet, filesPath(ref, ""), q, nil, "")
	if err != nil {
		return nil, err
	}
	return &TarStream{ReadCloser: body, trailer: trailer}, nil
}

// ImportTar extracts an archive below dest. The body streams: no temporary
// file stands between the caller's tar and the request.
func (c *Client) ImportTar(ctx context.Context, ref, dest string, body io.Reader) error {
	return c.write(ctx, http.MethodPut, filesPath(ref, ""), url.Values{"dest": []string{dest}}, body, tarMedia)
}

// write is a call whose answer carries nothing, which the file routes
// answer as 204. The body streams, so an upload of any size holds nothing
// in memory.
func (c *Client) write(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) error {
	req, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// StreamError reports a transfer that failed after the first byte, which is
// the one failure the API can no longer answer with a status: design 008
// says the trailer carries the code instead.
type StreamError struct {
	// Code is the server's code for the failure, where the trailer carried
	// one.
	Code string
	// Err is the transport's failure, where the stream was cut rather than
	// ended by the server.
	Err error
}

// Error names the failure.
func (e *StreamError) Error() string {
	if e.Code != "" {
		return "the transfer ended early: " + e.Code
	}
	return "the transfer ended early: " + e.Err.Error()
}

// Unwrap is the transport's failure.
func (e *StreamError) Unwrap() error { return e.Err }
