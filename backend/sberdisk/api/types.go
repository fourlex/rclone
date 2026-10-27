// Package api contains the types used by the SberDisk API.
package api

import "fmt"

// ErrorResponse is returned by SberDisk for non-2xx responses.
type ErrorResponse struct {
	StatusCode  int    `json:"-"`
	CodeID      int    `json:"codeId"`
	Code        string `json:"code"`
	Description string `json:"description"`
	Message     string `json:"message"`
}

// Error implements the error interface.
func (e *ErrorResponse) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return fmt.Sprintf("SberDisk error %s (%d)", e.Code, e.StatusCode)
	}
	return fmt.Sprintf("SberDisk HTTP error %d", e.StatusCode)
}

// File describes a file or folder.
type File struct {
	ID                  int64    `json:"id"`
	Name                string   `json:"name"`
	Ext                 *string  `json:"ext"`
	MimeType            string   `json:"mimeType"`
	Size                string   `json:"size"`
	Type                string   `json:"type"`
	SystemType          string   `json:"systemType"`
	FilePath            string   `json:"filePath"`
	Bytes               int64    `json:"bytes"`
	Preview             Preview  `json:"preview"`
	Groups              []string `json:"groups"`
	UploadAt            string   `json:"uploadAt"`
	CreatedAt           string   `json:"createdAt"`
	LastChangeAt        string   `json:"lastChangeAt"`
	LastChangeAuthor    string   `json:"lastChangeAuthor"`
	IsPrint             bool     `json:"isPrint"`
	IsFolder            bool     `json:"isFolder"`
	IsAppCategory       bool     `json:"isAppCategory"`
	DownloadURL         string   `json:"downloadUrl"`
	DownloadsAmount     string   `json:"downloadsAmount"`
	IsEditable          string   `json:"isEditable"`
	IsPasswordProtected bool     `json:"isPasswordProtected"`
	Location            any      `json:"location"`
}

// Preview contains the preview URLs for a file.
type Preview struct {
	IconS     string `json:"iconS"`
	IconL     string `json:"iconL"`
	TypeIcon  string `json:"typeIcon"`
	PosterURL string `json:"posterUrl"`
	Source    string `json:"source"`
}

// Links contains pagination links.
type Links struct {
	First *string `json:"first"`
	Last  *string `json:"last"`
	Prev  *string `json:"prev"`
	Next  *string `json:"next"`
}

// Meta contains pagination metadata.
type Meta struct {
	CurrentPage int `json:"current_page"`
	From        int `json:"from"`
	LastPage    int `json:"last_page"`
	PerPage     int `json:"per_page"`
	To          int `json:"to"`
	Total       int `json:"total"`
}

// ListResponse is returned by GET /files.
type ListResponse struct {
	Data  []File `json:"data"`
	Links Links  `json:"links"`
	Meta  Meta   `json:"meta"`
}

// CreateRequest is the body of POST /files/create.
type CreateRequest struct {
	FilePath   string `json:"filePath"`
	FileName   string `json:"fileName"`
	AutoRename bool   `json:"autoRename"`
}

// RenameRequest is the body of POST /files/rename/{id}.
type RenameRequest struct {
	FileName   string `json:"fileName"`
	AutoRename bool   `json:"autoRename"`
}

// FilePathPair is one entry in a copy or move request.
type FilePathPair struct {
	FromPath string `json:"fromPath"`
	ToPath   string `json:"toPath"`
}

// FileOperationRequest is the body of POST /files/copy and /files/move.
type FileOperationRequest struct {
	Files      []FilePathPair `json:"files"`
	AutoRename bool           `json:"autoRename"`
}

// DeleteRequest is the body of POST /files/delete.
type DeleteRequest struct {
	FileIDs []string `json:"fileIds"`
}

// OperationResponse is returned by some asynchronous operations.
type OperationResponse struct {
	OperationID string         `json:"operationId"`
	Payload     MessagePayload `json:"payload"`
}

// MessagePayload contains a human readable message.
type MessagePayload struct {
	Message string `json:"message"`
}

// MessageResponse is a simple response containing a message.
type MessageResponse struct {
	Message string `json:"message"`
}

// UploadFileRequest describes one file in an upload session request.
type UploadFileRequest struct {
	ID             int    `json:"id"`
	AutoRename     bool   `json:"autoRename"`
	Category       string `json:"category"`
	CreatesFolders bool   `json:"createsFolders"`
	FilePath       string `json:"filePath"`
	FileType       string `json:"fileType"`
	Size           int64  `json:"size"`
}

// UploadSessionsRequest is the body of POST /upload/multisessions.
type UploadSessionsRequest struct {
	Files []UploadFileRequest `json:"files"`
}

// UploadSession describes one upload session.
type UploadSession struct {
	ID        int    `json:"id"`
	SessionID string `json:"sessionId"`
	Size      int64  `json:"size"`
	FilePath  string `json:"filePath"`
	FileID    string `json:"fileId"`
}

// UploadSessionsResponse is returned by POST /upload/multisessions.
type UploadSessionsResponse struct {
	Sessions []UploadSession `json:"sessions"`
}

// SizeInfo contains a human readable and a numeric size.
type SizeInfo struct {
	Size  string `json:"size"`
	Bytes int64  `json:"bytes"`
}

// QuoteResponse is returned by GET /quote.
type QuoteResponse struct {
	Label      string              `json:"label"`
	All        SizeInfo            `json:"all"`
	Used       SizeInfo            `json:"used"`
	Categories map[string]SizeInfo `json:"categories"`
}

// Profile is returned by GET /profile.
type Profile struct {
	ID               int64  `json:"id"`
	FirstName        string `json:"firstName"`
	LastName         string `json:"lastName"`
	Email            string `json:"email"`
	Phone            string `json:"phone"`
	IsPro            bool   `json:"isPro"`
	PhoneVerificated bool   `json:"phoneVerificated"`
}
