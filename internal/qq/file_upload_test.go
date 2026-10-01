package qq

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fileUploadTransport struct {
	paths    []string
	bodies   []map[string]any
	chunks   [][]byte
	prepare  string
	fileInfo string
}

func (f *fileUploadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.paths = append(f.paths, r.URL.Path)
	response := `{}`
	if r.Method == http.MethodPut {
		data, _ := io.ReadAll(r.Body)
		f.chunks = append(f.chunks, data)
	} else {
		var body map[string]any
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
		}
		f.bodies = append(f.bodies, body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload_prepare"):
			response = f.prepare
		case strings.HasSuffix(r.URL.Path, "/files"):
			response = `{"file_info":"` + f.fileInfo + `"}`
		case strings.HasSuffix(r.URL.Path, "/messages"):
			response = `{"id":"image-message"}`
		}
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
}

func TestGroupAndC2CFileUploadPreserveImageAndReply(t *testing.T) {
	for _, group := range []bool{true, false} {
		transport := &fileUploadTransport{
			prepare:  `{"upload_id":"upload","block_size":"4","parts":[{"index":0,"presigned_url":"https://upload.test/0"},{"index":1,"presigned_url":"https://upload.test/1","block_size":"2"}]}`,
			fileInfo: "media",
		}
		client := &Client{httpClient: &http.Client{Transport: transport}, token: "token", expiresAt: time.Now().Add(time.Hour)}
		send := client.SendC2CFile
		prefix := "/v2/users/target"
		if group {
			send, prefix = client.SendGroupFile, "/v2/groups/target"
		}
		sent, err := send(context.Background(), "target", "incoming", "status.png", 1, []byte("abcdef"))
		if err != nil || sent.ID != "image-message" {
			t.Fatal(sent, err)
		}
		if len(transport.chunks) != 2 || string(transport.chunks[0]) != "abcd" || string(transport.chunks[1]) != "ef" {
			t.Fatal(transport.chunks)
		}
		if transport.paths[0] != prefix+"/upload_prepare" || transport.paths[len(transport.paths)-1] != prefix+"/messages" {
			t.Fatal(transport.paths)
		}
		last := transport.bodies[len(transport.bodies)-1]
		if last["msg_type"] != float64(7) || last["msg_id"] != "incoming" || last["msg_seq"] != float64(1) {
			t.Fatal(last)
		}
	}
}

func TestFileUploadUsesOfficialMD5PrefixAndRejectsEmptyMedia(t *testing.T) {
	data := bytes.Repeat([]byte("x"), (10<<20)+1)
	transport := &fileUploadTransport{
		prepare: `{"upload_id":"upload","block_size":20000000,"parts":[]}`,
	}
	client := &Client{httpClient: &http.Client{Transport: transport}, token: "token", expiresAt: time.Now().Add(time.Hour)}
	_, err := client.SendGroupFile(context.Background(), "group", "", "status.png", 1, data)
	if err == nil {
		t.Fatal("missing file_info accepted")
	}
	want := fmt.Sprintf("%x", md5.Sum(data[:10002432]))
	if transport.bodies[0]["md5_10m"] != want {
		t.Fatal(transport.bodies[0]["md5_10m"], want)
	}
	if _, err := client.SendC2CFile(context.Background(), "user", "", "empty.png", 1, nil); err == nil {
		t.Fatal("empty file accepted")
	}
}
