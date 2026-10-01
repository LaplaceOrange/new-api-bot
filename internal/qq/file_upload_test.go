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
			prepare:  `{"upload_id":"upload","block_size":"4","parts":[{"index":1,"presigned_url":"https://upload.test/1"},{"index":2,"presigned_url":"https://upload.test/2","block_size":"2"}]}`,
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

func TestSinglePartUploadUsesOneBasedOfficialIndex(t *testing.T) {
	transport := &fileUploadTransport{
		prepare:  `{"upload_id":"single","block_size":1048576,"parts":[{"index":1,"presigned_url":"https://upload.test/1"}]}`,
		fileInfo: "media",
	}
	client := &Client{httpClient: &http.Client{Transport: transport}, token: "token", expiresAt: time.Now().Add(time.Hour)}
	if _, err := client.SendGroupFile(context.Background(), "g", "incoming", "status.png", 1, []byte("png")); err != nil {
		t.Fatal(err)
	}
	if len(transport.chunks) != 1 || string(transport.chunks[0]) != "png" {
		t.Fatal("single image bytes changed", transport.chunks)
	}
}

func TestFileUploadUsesOfficialMD5PrefixAndRejectsEmptyMedia(t *testing.T) {
	data := bytes.Repeat([]byte("x"), (10<<20)+1)
	transport := &fileUploadTransport{
		prepare: `{"upload_id":"upload","block_size":20000000,"parts":[{"index":1,"presigned_url":"https://upload.test/1"}]}`,
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

func TestFileUploadReplySequenceDoesNotReuseProgressSequence(t *testing.T) {
	for _, group := range []bool{true, false} {
		transport := &fileUploadTransport{prepare: `{"upload_id":"upload","block_size":8,"parts":[{"index":1,"presigned_url":"https://upload.test/1"}]}`, fileInfo: "media"}
		client := &Client{httpClient: &http.Client{Transport: transport}, token: "token", expiresAt: time.Now().Add(time.Hour)}
		send := client.SendC2CFileWithSequence
		if group {
			send = client.SendGroupFileWithSequence
		}
		if _, err := send(context.Background(), "target", "incoming", "status.png", 1, []byte("png"), 3); err != nil {
			t.Fatal(err)
		}
		last := transport.bodies[len(transport.bodies)-1]
		if last["msg_seq"] != float64(3) || last["msg_id"] != "incoming" {
			t.Fatal(last)
		}
		count := len(transport.paths)
		if _, err := send(context.Background(), "target", "incoming", "status.png", 1, []byte("png"), 0); err == nil || len(transport.paths) != count {
			t.Fatal("invalid sequence sent")
		}
	}
}

func TestOfficialOneBasedMultipartOffsetsAndFinishMetadata(t *testing.T) {
	for _, group := range []bool{true, false} {
		for _, indexes := range [][]int{{1, 2, 3}, {3, 1, 2}} {
			for _, stringIndex := range []bool{false, true} {
				name := fmt.Sprintf("group=%v/indexes=%v/string=%v", group, indexes, stringIndex)
				t.Run(name, func(t *testing.T) {
					data := []byte("abcdefghij")
					parts := make([]map[string]any, 0, len(indexes))
					for _, index := range indexes {
						var encodedIndex any = index
						if stringIndex {
							encodedIndex = fmt.Sprint(index)
						}
						parts = append(parts, map[string]any{
							"index": encodedIndex, "presigned_url": fmt.Sprintf("https://upload.test/%d", index),
						})
					}
					prepare, _ := json.Marshal(map[string]any{
						"upload_id": "multipart", "block_size": "4", "parts": parts,
					})
					transport := &fileUploadTransport{prepare: string(prepare), fileInfo: "media"}
					client := &Client{httpClient: &http.Client{Transport: transport}, token: "token", expiresAt: time.Now().Add(time.Hour)}
					send := client.SendC2CFileWithSequence
					if group {
						send = client.SendGroupFileWithSequence
					}
					if _, err := send(context.Background(), "target", "incoming", "status.png", 1, data, 3); err != nil {
						t.Fatal(err)
					}
					if transport.bodies[0]["file_size"] != float64(len(data)) {
						t.Fatal("file_size is not numeric", transport.bodies[0])
					}
					if len(transport.chunks) != len(indexes) {
						t.Fatal("wrong number of parts", len(transport.chunks))
					}
					for i, index := range indexes {
						start := (index - 1) * 4
						want := data[start:min(start+4, len(data))]
						if !bytes.Equal(transport.chunks[i], want) {
							t.Fatalf("index=%d uploaded %q, want %q", index, transport.chunks[i], want)
						}
						finish := transport.bodies[i+1]
						if finish["part_index"] != float64(index) || finish["block_size"] != float64(len(want)) || finish["md5"] != fmt.Sprintf("%x", md5.Sum(want)) {
							t.Fatal("part_finish metadata changed", finish)
						}
					}
					complete := transport.bodies[len(indexes)+1]
					if len(complete) != 1 || complete["upload_id"] != "multipart" {
						t.Fatal("completion did not match SDK contract", complete)
					}
					last := transport.bodies[len(transport.bodies)-1]
					if last["msg_seq"] != float64(3) || last["msg_id"] != "incoming" {
						t.Fatal("upload repair changed reply sequence", last)
					}
				})
			}
		}
	}
}

func TestInvalidUploadPreparationCannotSendAnyPartOrFinalize(t *testing.T) {
	for _, prepare := range []string{
		`{"upload_id":"u","block_size":4,"parts":[]}`,
		`{"upload_id":"u","block_size":0,"parts":[{"index":1,"presigned_url":"https://upload.test/1"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":0,"presigned_url":"https://upload.test/0"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":-1,"presigned_url":"https://upload.test/1"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"presigned_url":"https://upload.test/1"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":3,"presigned_url":"https://upload.test/3"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":1,"presigned_url":"https://upload.test/1"},{"index":1,"presigned_url":"https://upload.test/2"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":1,"presigned_url":"https://upload.test/1"},{"index":3,"presigned_url":"https://upload.test/3"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":1,"presigned_url":"https://upload.test/1"},{"index":2,"block_size":1,"presigned_url":"https://upload.test/2"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":1,"block_size":-1,"presigned_url":"https://upload.test/1"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":1,"presigned_url":""}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":1,"presigned_url":"https://user:private-password@upload.test/1"}]}`,
		`{"upload_id":"u","block_size":4,"parts":[{"index":1,"presigned_url":"file:///private-password"}]}`,
	} {
		transport := &fileUploadTransport{prepare: prepare, fileInfo: "media"}
		client := &Client{httpClient: &http.Client{Transport: transport}, token: "token", expiresAt: time.Now().Add(time.Hour)}
		_, err := client.SendGroupFile(context.Background(), "target", "incoming", "status.png", 1, []byte("abcdef"))
		if err == nil || len(transport.paths) != 1 || len(transport.chunks) != 0 {
			t.Fatalf("invalid preparation performed uploads: err=%v paths=%v", err, transport.paths)
		}
		if strings.Contains(err.Error(), "private-password") {
			t.Fatal("validation leaked presigned credentials")
		}
	}
}
