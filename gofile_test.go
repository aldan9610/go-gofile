package gofile

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWebsiteToken checks the token algorithm against a vector computed
// independently (Python hashlib.sha256) for window 470000.
func TestWebsiteToken(t *testing.T) {
	now := time.Unix(int64(470000)*WTWindowSeconds, 0)
	got := WebsiteToken("testtoken", DefaultUserAgent, DefaultLanguage, DefaultSalt, 0, now)
	want := "5483f29c6eecd73770d7be4b11836c8a4c13107067042e886907146a5fedb393"
	if got != want {
		t.Fatalf("website token = %s, want %s", got, want)
	}
	// A different window must produce a different token.
	other := WebsiteToken("testtoken", DefaultUserAgent, DefaultLanguage, DefaultSalt, 1, now)
	if other == got {
		t.Fatal("window offset did not change the token")
	}
}

// TestRequestURLProxy checks the URL-prefix rewriting.
func TestRequestURLProxy(t *testing.T) {
	c := New(WithProxyPrefix("https://proxy.test/?"))
	target := "https://api.gofile.io/contents/abc?x=1"
	got := c.requestURL(target)
	want := "https://proxy.test/?" + url.QueryEscape(target)
	if got != want {
		t.Fatalf("requestURL = %s, want %s", got, want)
	}
	if got := New().requestURL(target); got != target {
		t.Fatalf("plain client must not rewrite URLs, got %s", got)
	}
}

// TestContentPagination folds paginated children into one Content.
func TestContentPagination(t *testing.T) {
	mux := http.NewServeMux()
	file := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"name":%q,"type":"file","size":1,"md5":"deadbeef"}`, id, id+".bin")
	}
	mux.HandleFunc("/contents/f1", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var body string
		if page == "1" {
			body = fmt.Sprintf(`{"status":"ok","data":{"id":"f1","name":"folder","type":"folder","childrenCount":3,
				"children":{"a":%s,"b":%s},
				"metadata":{"page":1,"totalPages":2,"hasNextPage":true,"totalCount":3}}}`, file("a"), file("b"))
		} else {
			body = fmt.Sprintf(`{"status":"ok","data":{"id":"f1","name":"folder","type":"folder","childrenCount":3,
				"children":{"c":%s},
				"metadata":{"page":2,"totalPages":2,"hasNextPage":false,"totalCount":3}}}`, file("c"))
		}
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := New(WithAPIBase(server.URL))
	content, err := client.Content(context.Background(), "f1", "")
	if err != nil {
		t.Fatal(err)
	}
	if content.Name != "folder" || len(content.Children) != 3 {
		t.Fatalf("unexpected content: %d children, name %q", len(content.Children), content.Name)
	}
	if child, ok := content.Children["c"]; !ok || child.Name != "c.bin" {
		t.Fatal("second page was not merged")
	}
}

// TestAPIErrorAndPremiumRetry checks error mapping and the previous-window
// retry on "error-notPremium".
func TestAPIErrorAndPremiumRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"status":"error-notPremium","data":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","data":{"id":"x","name":"x","type":"file"}}`))
	}))
	defer server.Close()

	client := New(WithAPIBase(server.URL), WithToken("tok"))
	content, err := client.Content(context.Background(), "x", "")
	if err != nil {
		t.Fatalf("expected the notPremium retry to succeed, got %v", err)
	}
	if content.ID != "x" || calls != 2 {
		t.Fatalf("calls = %d, content = %+v", calls, content)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error-notFound","data":{}}`))
	}))
	defer failing.Close()
	if _, err := New(WithAPIBase(failing.URL)).Content(context.Background(), "x", ""); err == nil {
		t.Fatal("expected an APIError")
	} else if apiErr, ok := err.(*APIError); !ok || apiErr.Status != StatusNotFound {
		t.Fatalf("unexpected error: %v", err)
	}
}

// rangeServer serves deterministic bytes with Range support, like gofile's
// store servers do.
func rangeServer(total int64, seed byte) (*httptest.Server, []byte, string) {
	data := make([]byte, total)
	for i := range data {
		data[i] = seed ^ byte(i%251)
	}
	sum := md5.Sum(data)
	mux := http.NewServeMux()
	mux.HandleFunc("/download/web/x/file.bin", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Redirect(w, r, "https://gofile.io/d/x", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", fmt.Sprint(total))
			_, _ = w.Write(data)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if start >= total {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= total {
			end = total - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
	return httptest.NewServer(mux), data, hex.EncodeToString(sum[:])
}

// TestChunkedDownload verifies chunked download, MD5 verification and resume.
func TestChunkedDownloadAndResume(t *testing.T) {
	const total = int64(3 << 20)
	server, data, wantMD5 := rangeServer(total, 0x5A)
	defer server.Close()

	client := New(WithToken("tok"))
	dest := filepath.Join(t.TempDir(), "file.bin")

	chunk := int64(1 << 20)
	err := client.DownloadToFile(context.Background(), server.URL+"/download/web/x/file.bin", dest,
		DownloadOptions{ChunkSize: chunk, MD5: wantMD5})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != total {
		t.Fatalf("size = %d, want %d", len(got), total)
	}
	for i := range data {
		if got[i] != data[i] {
			t.Fatalf("byte %d differs", i)
		}
	}

	// Resume: truncate the file to 1.5 MiB and download again.
	if err := os.Truncate(dest, (3<<20)/2); err != nil {
		t.Fatal(err)
	}
	err = client.DownloadToFile(context.Background(), server.URL+"/download/web/x/file.bin", dest,
		DownloadOptions{ChunkSize: chunk, MD5: wantMD5})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Size() != total {
		t.Fatalf("resumed size = %v, want %d", info, total)
	}

	// MD5 mismatch must be reported.
	err = client.DownloadToFile(context.Background(), server.URL+"/download/web/x/file.bin", dest,
		DownloadOptions{ChunkSize: chunk, MD5: "wrong"})
	var mismatch *ErrMD5Mismatch
	if err == nil {
		t.Fatal("expected an MD5 mismatch error")
	} else if !asMD5Error(err, &mismatch) {
		t.Fatalf("unexpected error type: %v", err)
	}

	// Wrong token gets redirected by the (fake) store.
	badClient := New(WithToken("nope"))
	if err := badClient.DownloadToFile(context.Background(), server.URL+"/download/web/x/file.bin", dest,
		DownloadOptions{ChunkSize: chunk}); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected a not-authorized error, got %v", err)
	}
}

func asMD5Error(err error, target **ErrMD5Mismatch) bool {
	if e, ok := err.(*ErrMD5Mismatch); ok {
		*target = e
		return true
	}
	return false
}

// TestStreamingDownload checks the no-chunks path.
func TestStreamingDownload(t *testing.T) {
	const total = int64(512 << 10)
	server, _, wantMD5 := rangeServer(total, 0x33)
	defer server.Close()

	client := New(WithToken("tok"))
	dest := filepath.Join(t.TempDir(), "file.bin")
	if err := client.DownloadToFile(context.Background(), server.URL+"/download/web/x/file.bin", dest,
		DownloadOptions{MD5: wantMD5}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dest); err != nil || info.Size() != total {
		t.Fatalf("streaming download size wrong: %v", info)
	}
}

// TestUpload checks multipart upload against a fake store server.
func TestUpload(t *testing.T) {
	payload := []byte("real payload bytes for the upload test")
	var (
		gotName  string
		gotBody  []byte
		gotAuth  string
		gotWT    string
		gotBound string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/servers" {
			_, _ = w.Write([]byte(`{"status":"ok","data":{"servers":[{"name":"srv","zone":"eu"}]}}`))
			return
		}
		if r.URL.Path != "/contents/uploadfile" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotWT = r.Header.Get("X-Website-Token")
		gotBound = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = file.Close() }()
		gotName = header.Filename
		gotBody, _ = io.ReadAll(file)
		if folder := r.FormValue("folderId"); folder != "folder-123" {
			t.Errorf("folderId = %q", folder)
		}
		sum := md5.Sum(gotBody)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"ok","data":{"id":"newfile","name":%q,"type":"file",
			"size":%d,"md5":%q,"servers":["srv"],"downloadPage":"https://gofile.io/d/abc"}}`,
			gotName, len(gotBody), hex.EncodeToString(sum[:]))))
	}))
	defer server.Close()

	client := New(WithAPIBase(server.URL), WithToken("tok"), WithServerBase(server.URL))
	res, err := client.Upload(context.Background(), strings.NewReader(string(payload)), "test.bin", "folder-123")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotWT == "" {
		t.Errorf("X-Website-Token missing")
	}
	if !strings.HasPrefix(gotBound, "multipart/form-data") {
		t.Errorf("Content-Type = %q", gotBound)
	}
	if gotName != "test.bin" || string(gotBody) != string(payload) {
		t.Errorf("upload mismatch: name %q, %d bytes", gotName, len(gotBody))
	}
	if res.ID != "newfile" || res.DownloadPage != "https://gofile.io/d/abc" {
		t.Errorf("unexpected result: %+v", res.Content)
	}
	if res.Content.DownloadURL() != "https://srv.gofile.io/download/web/newfile/test.bin" {
		t.Errorf("DownloadURL = %q", res.Content.DownloadURL())
	}
}

// TestTransientRetry checks that GET requests retry on transient 5xx.
func TestTransientRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(522)
			_, _ = w.Write([]byte("cloudflare error page"))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","data":{"id":"x","name":"x","type":"file"}}`))
	}))
	defer server.Close()

	client := New(WithAPIBase(server.URL), WithToken("tok"))
	content, err := client.Content(context.Background(), "x", "")
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if calls != 3 || content.ID != "x" {
		t.Fatalf("calls = %d", calls)
	}
}

// TestGuestUpload creates a guest account first when no token is set.
func TestGuestUpload(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","data":{"id":"acc-1","rootFolder":"rf","tier":"guest","token":"guest-token"}}`))
	})
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","data":{"servers":[{"name":"srv1","zone":"eu"}]}}`))
	})
	mux.HandleFunc("/contents/uploadfile", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"ok","data":{"id":"f","name":"n","type":"file","guestToken":"guest-token"}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := New(WithAPIBase(server.URL), WithServerBase(server.URL))
	res, err := client.Upload(context.Background(), strings.NewReader("x"), "n", "")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer guest-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if res.GuestToken != "guest-token" {
		t.Errorf("GuestToken = %q", res.GuestToken)
	}
}
