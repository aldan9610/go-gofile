// api.go
//
// Methods for the gofile API endpoints.
package gofile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Servers returns the list of available store servers.
func (c *Client) Servers(ctx context.Context) ([]Server, error) {
	var data ServersResponse
	if err := c.apiCall(ctx, http.MethodGet, c.fullURL("/servers"), nil, -1, "", &data); err != nil {
		return nil, err
	}
	return data.Servers, nil
}

// CreateGuestAccount creates a guest account and stores its token in the
// client for subsequent calls. It fails when the client already has a token.
func (c *Client) CreateGuestAccount(ctx context.Context) (*Account, error) {
	if c.token != "" {
		return nil, fmt.Errorf("gofile: client already has an account token")
	}
	var account Account
	if err := c.apiCall(ctx, http.MethodPost, c.fullURL("/accounts"), nil, -1, "", &account); err != nil {
		return nil, err
	}
	if account.Token == "" {
		return nil, fmt.Errorf("gofile: guest account response missing token")
	}
	c.token = account.Token
	return &account, nil
}

// GetAccountID resolves the authenticated token to its account (ID, email
// and tier are populated).
func (c *Client) GetAccountID(ctx context.Context) (*Account, error) {
	if c.token == "" {
		return nil, errNoToken
	}
	var account Account
	if err := c.apiCall(ctx, http.MethodGet, c.fullURL("/accounts/getid"), nil, -1, "", &account); err != nil {
		return nil, err
	}
	return &account, nil
}

// Account returns the account with the given ID. When accountID is empty the
// client resolves it from the token via GET /accounts/getid.
func (c *Client) Account(ctx context.Context, accountID string) (*Account, error) {
	if c.token == "" {
		return nil, errNoToken
	}
	if accountID == "" {
		resolved, err := c.GetAccountID(ctx)
		if err != nil {
			return nil, err
		}
		accountID = resolved.ID
	}
	var account Account
	if err := c.apiCall(ctx, http.MethodGet, c.fullURL("/accounts/"+url.PathEscape(accountID)), nil, -1, "", &account); err != nil {
		return nil, err
	}
	return &account, nil
}

// Content returns the file or folder with the given ID or code, following
// pagination until all children have been fetched.
// password (optional) hashes to the "password" query parameter for
// password-protected links.
func (c *Client) Content(ctx context.Context, idOrCode, password string) (*Content, error) {
	q := url.Values{
		"contentFilter": {""},
		"page":          {"1"},
		"pageSize":      {strconv.Itoa(c.pageSize)},
		"sortField":     {"createTime"},
		"sortDirection": {"-1"},
	}
	if password != "" {
		sum := sha256.Sum256([]byte(password))
		q.Set("password", hex.EncodeToString(sum[:]))
	}

	var result *Content
	var children map[string]Content
	page := 1
	for {
		q.Set("page", strconv.Itoa(page))
		var data contentsResponse
		if err := c.apiCall(ctx, http.MethodGet, c.fullURL("/contents/"+url.PathEscape(idOrCode))+"?"+q.Encode(), nil, -1, "", &data); err != nil {
			return nil, err
		}
		if result == nil {
			self := data.Content
			result = &self
		}
		if data.Children != nil {
			if children == nil {
				children = make(map[string]Content, data.ChildrenCount)
			}
			for k, v := range data.Children {
				children[k] = v
			}
		}
		if !data.Metadata.HasNextPage {
			break
		}
		page++
	}
	if children != nil {
		result.Children = children
		result.ChildrenCount = int64(len(children))
	}
	return result, nil
}

// DeleteContents removes the contents with the given IDs. The caller must own
// them (or be authenticated as their account). The response reports one
// status per ID; any failure becomes an error.
func (c *Client) DeleteContents(ctx context.Context, ids []string) error {
	if c.token == "" {
		return errNoToken
	}
	if len(ids) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"contentsId": strings.Join(ids, ",")})
	var results map[string]struct {
		Status string `json:"status"`
	}
	if err := c.apiCall(ctx, http.MethodDelete, c.fullURL("/contents"), bytes.NewReader(payload), int64(len(payload)), "application/json", &results); err != nil {
		return err
	}
	var failed []string
	for id, res := range results {
		if res.Status != "ok" {
			failed = append(failed, fmt.Sprintf("%s: %s", id, res.Status))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("gofile: delete failed for: %s", strings.Join(failed, "; "))
	}
	return nil
}

// Upload streams r (named name) to a gofile store server, optionally into an
// existing folder (folderID). When the client has no token, a guest account
// is created and used; the guest token is available in the result.
func (c *Client) Upload(ctx context.Context, r io.Reader, name, folderID string) (*UploadResult, error) {
	if c.token == "" {
		if _, err := c.CreateGuestAccount(ctx); err != nil {
			return nil, fmt.Errorf("gofile: creating guest account for upload: %w", err)
		}
	}
	servers, err := c.Servers(ctx)
	if err != nil {
		return nil, fmt.Errorf("gofile: listing servers: %w", err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("gofile: no upload server available")
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	go func() {
		var werr error
		defer func() {
			if werr != nil {
				_ = pw.CloseWithError(werr)
			} else {
				_ = pw.Close()
			}
		}()
		if folderID != "" {
			if werr = mw.WriteField("folderId", folderID); werr != nil {
				return
			}
		}
		var part io.Writer
		part, werr = mw.CreateFormFile("file", name)
		if werr != nil {
			return
		}
		if _, werr = io.Copy(part, r); werr != nil {
			return
		}
		werr = mw.Close()
	}()

	target := c.storeURL(servers[0].Name, "/contents/uploadfile")
	var result UploadResult
	if err := c.apiCall(ctx, http.MethodPost, target, pr, -1, contentType, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DownloadURL returns the canonical direct-download URL of a file. When the
// API did not provide a link (e.g. in upload responses) it is constructed
// from the assigned server.
func (f *Content) DownloadURL() string {
	if f.Link != "" {
		return f.Link
	}
	if len(f.Servers) > 0 {
		return serverURL(f.Servers[0], "/download/web/"+f.ID+"/"+f.Name)
	}
	return ""
}
