// models.go
//
// Data types returned by the gofile API.
package gofile

// Server is an upload/download server as returned by GET /servers.
type Server struct {
	Name string `json:"name"`
	Zone string `json:"zone"`
}

// ServersResponse is the data payload of GET /servers.
type ServersResponse struct {
	Servers []Server `json:"servers"`
}

// Account is the data payload of GET /accounts/{accountId} (and of
// POST /accounts when a guest account is created; in that case only ID,
// RootFolder, Tier and Token are populated).
type Account struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Tier       string `json:"tier"`
	RootFolder string `json:"rootFolder"`
	Token      string `json:"token"`
}

// Content describes a file or a folder as returned by GET /contents/{id}
// and by the upload endpoint. The Type field is "file" or "folder".
type Content struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	Type           string             `json:"type"`
	Code           string             `json:"code"`
	ParentFolder   string             `json:"parentFolder"`
	Size           int64              `json:"size"`
	CreateTime     int64              `json:"createTime"`
	ModTime        int64              `json:"modTime"`
	MD5            string             `json:"md5"`
	Mimetype       string             `json:"mimetype"`
	Public         bool               `json:"public"`
	CanAccess      bool               `json:"canAccess"`
	DownloadCount  int64              `json:"downloadCount"`
	ChildrenCount  int64              `json:"childrenCount"`
	Servers        []string           `json:"servers"`
	ServerSelected string             `json:"serverSelected"`
	Link           string             `json:"link"`
	Children       map[string]Content `json:"children"`
}

// IsFolder reports whether the content is a folder.
func (c *Content) IsFolder() bool { return c.Type == "folder" }

// IsFile reports whether the content is a file.
func (c *Content) IsFile() bool { return c.Type == "file" }

// PageMetadata is the pagination info attached to GET /contents/{id}.
type PageMetadata struct {
	TotalCount  int  `json:"totalCount"`
	TotalPages  int  `json:"totalPages"`
	Page        int  `json:"page"`
	PageSize    int  `json:"pageSize"`
	HasNextPage bool `json:"hasNextPage"`
}

// contentsResponse is the data payload of GET /contents/{id}.
type contentsResponse struct {
	Content
	ChildrenCount int64             `json:"childrenCount"`
	Children      map[string]Content `json:"children"`
	Metadata      PageMetadata      `json:"metadata"`
}

// UploadResult is the data payload of the upload endpoint. It embeds the
// uploaded file's Content plus fields only present on upload.
type UploadResult struct {
	Content
	GuestToken       string `json:"guestToken"`
	DownloadPage     string `json:"downloadPage"`
	ParentFolderCode string `json:"parentFolderCode"`
}
