// download.go
//
// Download helpers for files hosted on gofile store servers.
//
// Store downloads require the same headers as the API (user agent, X-BL and
// the website token, plus the account token). Range requests are supported,
// which allows resuming and — through in-memory buffering proxies — fetching
// big files in chunks instead of one huge response.
package gofile

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

// DownloadOptions tunes DownloadToFile.
type DownloadOptions struct {
	// ChunkSize limits each request to this many bytes (Range requests) and
	// appends chunks to the destination, resuming from its current size.
	// 0 downloads with a single streaming request. Recommended when going
	// through a buffering proxy: 64 MiB is a safe value.
	ChunkSize int64
	// MD5 is the expected content hash; when non-empty the downloaded file
	// is verified against it (ErrMD5Mismatch on difference).
	MD5 string
	// Progress reports copied bytes (written, total). Total may be 0 when
	// unknown. Optional.
	Progress func(written, total int64)
	// Retries per chunk. Defaults to 3.
	Retries int
}

// DownloadToFile downloads the file at fileURL into dest. The caller obtains
// fileURL from Content.DownloadURL().
func (c *Client) DownloadToFile(ctx context.Context, fileURL string, dest string, opts DownloadOptions) error {
	if fileURL == "" {
		return fmt.Errorf("gofile: empty download URL")
	}
	if opts.Retries <= 0 {
		opts.Retries = 3
	}

	if opts.ChunkSize <= 0 {
		return c.downloadStream(ctx, fileURL, dest, opts)
	}
	return c.downloadChunked(ctx, fileURL, dest, opts)
}

// headersForDownload builds the header set used on store downloads.
func (c *Client) headersForDownload(token string) http.Header {
	h := make(http.Header)
	c.setHeaders(h, token, 0)
	return h
}

// downloadClient returns a copy of the underlying HTTP client that does not
// follow redirects: a 302 on a store download means "not authorized", and
// following it would fetch an HTML page — or hang when the redirect target
// is unreachable.
func (c *Client) downloadClient() http.Client {
	client := *c.http
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

// downloadStream performs a single streaming GET, writing to dest.part and
// renaming it into place on success.
func (c *Client) downloadStream(ctx context.Context, fileURL, dest string, opts DownloadOptions) error {
	h := c.headersForDownload(c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.requestURL(fileURL), nil)
	if err != nil {
		return err
	}
	copyHeaders(req.Header, h)
	dc := c.downloadClient()
	resp, err := dc.Do(req)
	if err != nil {
		return fmt.Errorf("gofile: download request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusFound {
		return fmt.Errorf("gofile: server redirected the download (not authorized?): %s",
			resp.Header.Get("Location"))
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gofile: unexpected HTTP %d on download", resp.StatusCode)
	}

	total := resp.ContentLength
	return copyWithHash(resp.Body, dest, opts, total)
}

// downloadChunked fetches the file in Range chunks, appending to dest and
// resuming from its current size.
func (c *Client) downloadChunked(ctx context.Context, fileURL, dest string, opts DownloadOptions) error {
	// Discover total size with a 1-byte probe.
	total, err := c.probeSize(ctx, fileURL)
	if err != nil {
		return err
	}
	if opts.Progress != nil {
		opts.Progress(0, total)
	}

	offset, err := resumeOffset(dest, total)
	if err != nil {
		return err
	}
	if opts.Progress != nil {
		opts.Progress(offset, total)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	hash := md5.New()
	if offset > 0 {
		// Hash the already downloaded prefix so the final check covers the
		// whole file even after a resume.
		if err := hashPrefix(offset, dest, hash); err != nil {
			return err
		}
	}

	for offset < total {
		end := offset + opts.ChunkSize - 1
		if end >= total {
			end = total - 1
		}
		n, err := c.fetchRange(ctx, fileURL, offset, end, f, hash, opts.Retries)
		if err != nil {
			return err
		}
		offset += n
		if opts.Progress != nil {
			opts.Progress(offset, total)
		}
	}

	got := hex.EncodeToString(hash.Sum(nil))
	if opts.MD5 != "" && got != opts.MD5 {
		return &ErrMD5Mismatch{Expected: opts.MD5, Got: got}
	}
	return nil
}

// probeSize issues a 1-byte Range request to learn the file size from the
// Content-Range header.
func (c *Client) probeSize(ctx context.Context, fileURL string) (int64, error) {
	h := c.headersForDownload(c.token)
	h.Set("Range", "bytes=0-0")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.requestURL(fileURL), nil)
	if err != nil {
		return 0, err
	}
	copyHeaders(req.Header, h)
	dc := c.downloadClient()
	resp, err := dc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gofile: probing download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusFound {
		return 0, fmt.Errorf("gofile: download not authorized (redirected to %s)",
			resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("gofile: unexpected HTTP %d probing size", resp.StatusCode)
	}
	// Content-Range: bytes 0-0/1317985506
	cr := resp.Header.Get("Content-Range")
	if i := lastSlash(cr); i >= 0 {
		size, perr := strconv.ParseInt(cr[i+1:], 10, 64)
		if perr != nil || size <= 0 {
			return 0, fmt.Errorf("gofile: cannot parse Content-Range %q", cr)
		}
		return size, nil
	}
	return 0, fmt.Errorf("gofile: missing Content-Range header")
}

// fetchRange downloads bytes [start,end] appending to f and hashing into
// hash. It returns the number of bytes appended.
func (c *Client) fetchRange(ctx context.Context, fileURL string, start, end int64, f *os.File, hash io.Writer, retries int) (int64, error) {
	expect := end - start + 1
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		h := c.headersForDownload(c.token)
		h.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.requestURL(fileURL), nil)
		if err != nil {
			return 0, err
		}
		copyHeaders(req.Header, h)
		dc := c.downloadClient()
		resp, err := dc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusFound {
			_ = resp.Body.Close()
			return 0, fmt.Errorf("gofile: download not authorized (redirected to %s)",
				resp.Header.Get("Location"))
		}
		if resp.StatusCode != http.StatusPartialContent {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("gofile: unexpected HTTP %d on range %d-%d: %s",
				resp.StatusCode, start, end, string(data))
			continue
		}
		n, cerr := io.Copy(io.MultiWriter(f, hash), resp.Body)
		_ = resp.Body.Close()
		if cerr != nil {
			lastErr = cerr
			continue
		}
		if n != expect {
			lastErr = fmt.Errorf("gofile: short chunk: got %d of %d bytes", n, expect)
			// Roll back f in case of a partial append.
			if n > 0 {
				if t := truncateTo(f, start); t != nil {
					return 0, t
				}
			}
			continue
		}
		return n, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("gofile: range %d-%d failed", start, end)
	}
	return 0, fmt.Errorf("gofile: range %d-%d: %w", start, end, lastErr)
}

// truncateTo rolls the file back to the given size (undo a partial chunk).
func truncateTo(f *os.File, size int64) error {
	if err := f.Truncate(size); err != nil {
		return err
	}
	_, err := f.Seek(size, io.SeekStart)
	return err
}

// resumeOffset returns the offset to resume from: the current size of dest
// when it is a valid prefix (smaller than total), otherwise 0 after
// truncating an oversize file.
func resumeOffset(dest string, total int64) (int64, error) {
	info, err := os.Stat(dest)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Size() > total {
		// Corrupted leftovers: start over.
		if err := os.Remove(dest); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return info.Size(), nil
}

// hashPrefix feeds the first size bytes of an already downloaded file into
// the running hash (resume support).
func hashPrefix(prefixLen int64, path string, hash io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.CopyN(hash, f, prefixLen)
	return err
}

// copyWithHash streams src into dest.part, hashing on the fly, then renames
// the file into dest and verifies the MD5.
func copyWithHash(src io.Reader, dest string, opts DownloadOptions, total int64) error {
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	hash := md5.New()
	written, err := io.Copy(io.MultiWriter(f, hash), src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gofile: writing download: %w", err)
	}
	if opts.Progress != nil {
		opts.Progress(written, total)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if opts.MD5 != "" && got != opts.MD5 {
		_ = os.Remove(tmp)
		return &ErrMD5Mismatch{Expected: opts.MD5, Got: got}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// lastSlash returns the index of the last "/" in s, or -1.
func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// copyHeaders copies all values from src into dst.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
