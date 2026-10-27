// Package sberdisk provides an interface to the SberDisk storage system.
package sberdisk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/sberdisk/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const (
	apiURL      = "https://apigw.sberdisk.ru"
	uploadURL   = "https://upload.sberdisk.ru"
	downloadURL = "https://download.sberdisk.ru"

	minSleep      = 10 * time.Millisecond
	maxSleep      = 2 * time.Second
	decayConstant = 2
	listLimit     = 1000
	waitAttempts  = 240
	waitInterval  = 250 * time.Millisecond
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "sberdisk",
		Description: "SberDisk",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name: "cookie",
			Help: `SberDisk session cookie.

Log in at https://files.sberdisk.ru, open the browser developer tools,
find a request to https://apigw.sberdisk.ru, and copy the full Cookie
request header (all cookies, not just _sa).`,
			Required:   true,
			IsPassword: true,
			Sensitive:  true,
		}, {
			Name:     "timezone",
			Help:     "Timezone header sent to SberDisk.",
			Default:  "Europe/Moscow",
			Advanced: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default: (encoder.Display |
				encoder.EncodeInvalidUtf8),
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	Cookie   string               `config:"cookie"`
	TimeZone string               `config:"timezone"`
	Enc      encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a remote SberDisk.
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	srv      *rest.Client
	pacer    *fs.Pacer
}

// Object describes a SberDisk object.
type Object struct {
	fs          *Fs
	remote      string
	id          int64
	name        string
	size        int64
	modTime     time.Time
	mimeType    string
	downloadURL string
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("SberDisk %s", f.root)
}

// Precision returns the precision of the modification times.
//
// SberDisk does not allow the modification time to be set, so rclone
// treats it as a backend without modtime support and compares by size.
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features {
	return f.features
}

// setRoot normalises the rclone root path.
func (f *Fs) setRoot(root string) {
	f.root = strings.Trim(root, "/")
}

// absPath returns the absolute SberDisk path for a remote path.
func (f *Fs) absPath(remote string) string {
	return path.Join("/", f.opt.Enc.FromStandardPath(f.root), f.opt.Enc.FromStandardPath(remote))
}

// readCookies parses a Cookie header or a bare _sa value.
// revealCookie accepts either an obscured config value or a raw cookie.
func revealCookie(raw string) string {
	revealed, err := obscure.Reveal(raw)
	if err != nil {
		return raw
	}
	for _, r := range revealed {
		if r < 0x20 || r > 0x7e {
			return raw
		}
	}
	return revealed
}

func readCookies(raw string) []*http.Cookie {
	raw = strings.TrimSpace(revealCookie(raw))
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "cookie:") {
		raw = strings.TrimSpace(raw[7:])
	}
	raw = strings.ReplaceAll(raw, "\n", "; ")
	if !strings.Contains(raw, "=") {
		raw = "_sa=" + raw
	}
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Add("Cookie", raw)
	return req.Cookies()
}

// NewFs constructs an Fs from the path, container:path.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}

	cookies := readCookies(opt.Cookie)
	if len(cookies) == 0 {
		return nil, errors.New("sberdisk: cookie is required")
	}

	f := &Fs{
		name: name,
		opt:  *opt,
	}
	f.setRoot(root)

	timezone := opt.TimeZone
	if timezone == "" {
		timezone = "Europe/Moscow"
	}

	f.srv = rest.NewClient(fshttp.NewClient(ctx)).SetRoot(apiURL)
	f.srv.SetCookie(cookies...)
	f.srv.SetHeader("Language", "ru")
	f.srv.SetHeader("Platform", "web")
	f.srv.SetHeader("TimeZone", timezone)
	f.srv.SetHeader("Origin", "https://files.sberdisk.ru")
	f.srv.SetHeader("Referer", "https://files.sberdisk.ru/")
	f.srv.SetErrorHandler(errorHandler)
	f.pacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(minSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant),
	))

	f.features = (&fs.Features{
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	f.features.Copy = f.Copy
	f.features.Move = f.Move
	f.features.DirMove = f.DirMove
	f.features.Purge = f.Purge
	f.features.About = f.About

	if _, err := f.getProfile(ctx); err != nil {
		return nil, fmt.Errorf("sberdisk: authentication failed: %w", err)
	}

	// If the root points at a file, return the parent as the root and let
	// rclone handle the file specially.
	if f.root != "" {
		item, err := f.findEntry(ctx, f.absPath(""))
		if err != nil {
			if errors.Is(err, fs.ErrorObjectNotFound) {
				// The root doesn't exist yet. rclone may be about to
				// create it (for example with `rclone mkdir`).
				return f, nil
			}
			return nil, fmt.Errorf("sberdisk: root %q: %w", root, err)
		}
		if !item.IsFolder {
			rootDir := path.Dir(f.root)
			if rootDir == "." {
				rootDir = ""
			}
			f.setRoot(rootDir)
			return f, fs.ErrorIsFile
		}
	}

	return f, nil
}

// callJSON runs a JSON API call with pacing and retries.
func (f *Fs) callJSON(ctx context.Context, opts *rest.Opts, request, response any) (*http.Response, error) {
	var resp *http.Response
	err := f.pacer.Call(func() (bool, error) {
		var callErr error
		resp, callErr = f.srv.CallJSON(ctx, opts, request, response)
		return shouldRetry(ctx, resp, callErr)
	})
	return resp, err
}

// call runs an API call with pacing and retries.
func (f *Fs) call(ctx context.Context, opts *rest.Opts) (*http.Response, error) {
	var resp *http.Response
	err := f.pacer.Call(func() (bool, error) {
		var callErr error
		resp, callErr = f.srv.Call(ctx, opts)
		return shouldRetry(ctx, resp, callErr)
	})
	return resp, err
}

func (f *Fs) getProfile(ctx context.Context) (*api.Profile, error) {
	var profile api.Profile
	opts := rest.Opts{Method: "GET", Path: "/profile"}
	_, err := f.callJSON(ctx, &opts, nil, &profile)
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// listDir lists one directory, following pagination.
func (f *Fs) listDir(ctx context.Context, parent string) ([]api.File, error) {
	var out []api.File
	for page := 1; ; page++ {
		opts := rest.Opts{
			Method:     "GET",
			Path:       "/files",
			Parameters: url.Values{},
		}
		opts.Parameters.Set("cnt", strconv.Itoa(listLimit))
		opts.Parameters.Set("page", strconv.Itoa(page))
		opts.Parameters.Set("parent", parent)
		opts.Parameters.Set("sort", "date")
		opts.Parameters.Set("direction", "desc")

		var response api.ListResponse
		_, err := f.callJSON(ctx, &opts, nil, &response)
		if err != nil {
			return nil, err
		}
		out = append(out, response.Data...)
		if len(response.Data) == 0 || response.Meta.LastPage == 0 || response.Meta.CurrentPage >= response.Meta.LastPage {
			break
		}
	}
	return out, nil
}

// findEntry finds one entry by its absolute path.
func (f *Fs) findEntry(ctx context.Context, abs string) (*api.File, error) {
	parent := path.Dir(abs)
	name := path.Base(abs)
	items, err := f.listDir(ctx, parent)
	if err != nil {
		if isNotFound(err) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	for i := range items {
		if items[i].Name == name {
			return &items[i], nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// findItemByID finds an entry by ID in a directory.
func (f *Fs) findItemByID(ctx context.Context, parent string, id int64) (*api.File, error) {
	items, err := f.listDir(ctx, parent)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == id {
			return &items[i], nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// listIDs returns the IDs in a directory.
func (f *Fs) listIDs(ctx context.Context, parent string) (map[string]struct{}, error) {
	items, err := f.listDir(ctx, parent)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{}, len(items))
	for _, item := range items {
		ids[strconv.FormatInt(item.ID, 10)] = struct{}{}
	}
	return ids, nil
}

func (f *Fs) newObject(remote string, item *api.File) *Object {
	return &Object{
		fs:          f,
		remote:      remote,
		id:          item.ID,
		name:        item.Name,
		size:        item.Bytes,
		modTime:     parseTime(item),
		mimeType:    item.MimeType,
		downloadURL: item.DownloadURL,
	}
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	items, err := f.listDir(ctx, f.absPath(dir))
	if err != nil {
		if isNotFound(err) {
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}

	entries := make(fs.DirEntries, 0, len(items))
	for i := range items {
		item := &items[i]
		remote := path.Join(dir, f.opt.Enc.ToStandardName(item.Name))
		if item.IsFolder {
			entries = append(entries, fs.NewDir(remote, parseTime(item)).SetID(strconv.FormatInt(item.ID, 10)))
		} else {
			entries = append(entries, f.newObject(remote, item))
		}
	}
	return entries, nil
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	item, err := f.findEntry(ctx, f.absPath(remote))
	if err != nil {
		return nil, err
	}
	if item.IsFolder {
		return nil, fs.ErrorIsDir
	}
	return f.newObject(remote, item), nil
}

// Put uploads a file to SberDisk.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	size := src.Size()
	if size < 0 {
		return nil, errors.New("sberdisk: can't upload a file with unknown size")
	}
	filePath := f.absPath(src.Remote())
	parent := path.Dir(filePath)
	name := path.Base(filePath)

	var existingID int64
	if item, err := f.findEntry(ctx, filePath); err == nil {
		if item.IsFolder {
			return nil, fs.ErrorIsDir
		}
		existingID = item.ID
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return nil, err
	}

	before, err := f.listIDs(ctx, parent)
	if err != nil && !isNotFound(err) {
		return nil, err
	}

	mimeType := fs.MimeType(ctx, src)
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	session, err := f.createUploadSession(ctx, filePath, mimeType, size, true)
	if err != nil {
		return nil, err
	}
	if err := f.uploadFile(ctx, session.SessionID, in, size, mimeType); err != nil {
		return nil, err
	}

	var item *api.File
	if id, err := strconv.ParseInt(session.FileID, 10, 64); err == nil {
		item, err = f.waitForItemByID(ctx, parent, id)
	} else {
		item, err = f.waitForNewItem(ctx, parent, before)
	}
	if err != nil {
		return nil, err
	}

	// The upload used autoRename to avoid clobbering the old file while
	// it was in progress. Now that it has completed, replace the old file
	// and give the new one the requested name.
	if existingID != 0 {
		if err := f.deleteIDs(ctx, []string{strconv.FormatInt(existingID, 10)}); err != nil {
			return nil, err
		}
	}
	if item.Name != name {
		if err := f.rename(ctx, item.ID, name); err != nil {
			return nil, err
		}
		item.Name = name
	}
	return f.newObject(src.Remote(), item), nil
}

func (f *Fs) createUploadSession(ctx context.Context, filePath, mimeType string, size int64, autoRename bool) (*api.UploadSession, error) {
	request := api.UploadSessionsRequest{
		Files: []api.UploadFileRequest{{
			ID:             1,
			AutoRename:     autoRename,
			Category:       "Autoload",
			CreatesFolders: true,
			FilePath:       filePath,
			FileType:       mimeType,
			Size:           size,
		}},
	}
	opts := rest.Opts{Method: "POST", RootURL: uploadURL, Path: "/upload/multisessions"}
	var response api.UploadSessionsResponse
	if _, err := f.callJSON(ctx, &opts, &request, &response); err != nil {
		return nil, err
	}
	if len(response.Sessions) == 0 {
		return nil, errors.New("sberdisk: no upload session returned")
	}
	return &response.Sessions[0], nil
}

func (f *Fs) uploadFile(ctx context.Context, sessionID string, in io.Reader, size int64, mimeType string) error {
	opts := rest.Opts{
		Method:               "POST",
		RootURL:              uploadURL,
		Path:                 "/upload/file/" + sessionID,
		Body:                 in,
		ContentLength:        &size,
		MultipartParams:      url.Values{"path": {""}},
		MultipartContentName: "file",
		MultipartFileName:    "blob",
		MultipartContentType: mimeType,
	}
	var response api.MessageResponse
	_, err := f.srv.CallJSON(ctx, &opts, nil, &response)
	if err != nil {
		return fmt.Errorf("sberdisk: upload failed: %w", err)
	}
	return nil
}

// Mkdir makes a directory and its parents.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return f.mkdirAPIPath(ctx, f.absPath(dir))
}

func (f *Fs) mkdirAPIPath(ctx context.Context, abs string) error {
	if abs == "/" || abs == "" {
		return nil
	}
	if item, err := f.findEntry(ctx, abs); err == nil {
		if item.IsFolder {
			return nil
		}
		return fs.ErrorIsFile
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}

	parent := path.Dir(abs)
	if err := f.mkdirAPIPath(ctx, parent); err != nil {
		return err
	}

	request := api.CreateRequest{
		FilePath:   parent,
		FileName:   path.Base(abs),
		AutoRename: false,
	}
	opts := rest.Opts{Method: "POST", Path: "/files/create"}
	var response api.OperationResponse
	if _, err := f.callJSON(ctx, &opts, &request, &response); err != nil {
		if item, checkErr := f.findEntry(ctx, abs); checkErr == nil && item.IsFolder {
			return nil
		}
		return err
	}
	return nil
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	abs := f.absPath(dir)
	if abs == "/" {
		return fs.ErrorCantPurge
	}
	item, err := f.findEntry(ctx, abs)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return fs.ErrorDirNotFound
		}
		return err
	}
	if !item.IsFolder {
		return fs.ErrorIsFile
	}
	items, err := f.listDir(ctx, abs)
	if err != nil {
		return err
	}
	if len(items) != 0 {
		return fs.ErrorDirectoryNotEmpty
	}
	return f.deleteIDs(ctx, []string{strconv.FormatInt(item.ID, 10)})
}

// Purge removes a directory and all its contents.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	abs := f.absPath(dir)
	if abs == "/" {
		items, err := f.listDir(ctx, "/")
		if err != nil {
			return err
		}
		for i := range items {
			if err := f.purgeItem(ctx, &items[i]); err != nil {
				return err
			}
		}
		return nil
	}

	item, err := f.findEntry(ctx, abs)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return fs.ErrorDirNotFound
		}
		return err
	}
	if !item.IsFolder {
		return fs.ErrorIsFile
	}
	return f.purgeItem(ctx, item)
}

func (f *Fs) purgeItem(ctx context.Context, item *api.File) error {
	if item.IsFolder {
		folderPath := path.Join(item.FilePath, item.Name)
		children, err := f.listDir(ctx, folderPath)
		if err != nil {
			return err
		}
		for i := range children {
			if err := f.purgeItem(ctx, &children[i]); err != nil {
				return err
			}
		}
	}
	err := f.deleteIDs(ctx, []string{strconv.FormatInt(item.ID, 10)})
	if isNotFound(err) {
		return nil
	}
	return err
}

// Copy copies src to remote using the SberDisk copy API.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantCopy
	}
	return f.copyOrMove(ctx, "copy", srcObj, remote)
}

// Move moves src to remote using the SberDisk move API.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}
	return f.copyOrMove(ctx, "move", srcObj, remote)
}

func (f *Fs) copyOrMove(ctx context.Context, op string, src *Object, remote string) (fs.Object, error) {
	srcPath := src.fullPath()
	dstPath := f.absPath(remote)
	if srcPath == dstPath {
		return src, nil
	}
	dstDir := path.Dir(dstPath)
	dstName := path.Base(dstPath)
	if err := f.mkdirAPIPath(ctx, dstDir); err != nil {
		return nil, err
	}
	if err := f.deleteIfExists(ctx, dstPath); err != nil {
		return nil, err
	}

	before, err := f.listIDs(ctx, dstDir)
	if err != nil {
		return nil, err
	}

	request := api.FileOperationRequest{
		Files: []api.FilePathPair{{
			FromPath: srcPath,
			ToPath:   dstDir,
		}},
		AutoRename: true,
	}
	apiPath := "/files/copy"
	if op == "move" {
		apiPath = "/files/move"
	}
	opts := rest.Opts{Method: "POST", Path: apiPath}
	var response api.OperationResponse
	if _, err := f.callJSON(ctx, &opts, &request, &response); err != nil {
		return nil, err
	}

	var item *api.File
	if op == "copy" {
		item, err = f.waitForNewItem(ctx, dstDir, before)
	} else {
		item, err = f.waitForItemByID(ctx, dstDir, src.id)
	}
	if err != nil {
		return nil, err
	}
	if item.Name != dstName {
		if err := f.rename(ctx, item.ID, dstName); err != nil {
			return nil, err
		}
	}
	return f.waitForObject(ctx, remote)
}

func (f *Fs) deleteIfExists(ctx context.Context, abs string) error {
	item, err := f.findEntry(ctx, abs)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil
		}
		return err
	}
	if item.IsFolder {
		return fs.ErrorIsDir
	}
	return f.deleteIDs(ctx, []string{strconv.FormatInt(item.ID, 10)})
}

// DirMove moves a directory.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}
	srcPath := srcFs.absPath(srcRemote)
	dstPath := f.absPath(dstRemote)
	if srcPath == dstPath {
		return nil
	}

	srcItem, err := srcFs.findEntry(ctx, srcPath)
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return fs.ErrorDirNotFound
		}
		return err
	}
	if !srcItem.IsFolder {
		return fs.ErrorIsFile
	}
	if _, err := f.findEntry(ctx, dstPath); err == nil {
		return fs.ErrorDirExists
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}

	dstDir := path.Dir(dstPath)
	dstName := path.Base(dstPath)
	if err := f.mkdirAPIPath(ctx, dstDir); err != nil {
		return err
	}

	request := api.FileOperationRequest{
		Files: []api.FilePathPair{{
			FromPath: srcPath,
			ToPath:   dstDir,
		}},
		AutoRename: true,
	}
	opts := rest.Opts{Method: "POST", Path: "/files/move"}
	var response api.OperationResponse
	if _, err := f.callJSON(ctx, &opts, &request, &response); err != nil {
		return err
	}

	item, err := f.waitForItemByID(ctx, dstDir, srcItem.ID)
	if err != nil {
		return err
	}
	if item.Name != dstName {
		return f.rename(ctx, item.ID, dstName)
	}
	return nil
}

func (f *Fs) rename(ctx context.Context, id int64, name string) error {
	request := api.RenameRequest{FileName: name, AutoRename: false}
	opts := rest.Opts{Method: "POST", Path: "/files/rename/" + strconv.FormatInt(id, 10)}
	var response api.OperationResponse
	_, err := f.callJSON(ctx, &opts, &request, &response)
	return err
}

func (f *Fs) deleteIDs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	request := api.DeleteRequest{FileIDs: ids}
	opts := rest.Opts{Method: "POST", Path: "/files/delete"}
	var response api.OperationResponse
	_, err := f.callJSON(ctx, &opts, &request, &response)
	return err
}

func (f *Fs) waitForItemByID(ctx context.Context, parent string, id int64) (*api.File, error) {
	var lastErr error
	for i := 0; i < waitAttempts; i++ {
		item, err := f.findItemByID(ctx, parent, id)
		if err == nil {
			return item, nil
		}
		lastErr = err
		if !errors.Is(err, fs.ErrorObjectNotFound) {
			break
		}
		if !sleep(ctx, waitInterval) {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = fs.ErrorObjectNotFound
	}
	return nil, lastErr
}

func (f *Fs) waitForNewItem(ctx context.Context, parent string, before map[string]struct{}) (*api.File, error) {
	for i := 0; i < waitAttempts; i++ {
		items, err := f.listDir(ctx, parent)
		if err != nil {
			return nil, err
		}
		for j := range items {
			if _, found := before[strconv.FormatInt(items[j].ID, 10)]; !found {
				return &items[j], nil
			}
		}
		if !sleep(ctx, waitInterval) {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("sberdisk: operation did not create a new file")
}

func (f *Fs) waitForObject(ctx context.Context, remote string) (fs.Object, error) {
	var lastErr error
	for i := 0; i < waitAttempts; i++ {
		obj, err := f.NewObject(ctx, remote)
		if err == nil {
			return obj, nil
		}
		lastErr = err
		if !errors.Is(err, fs.ErrorObjectNotFound) {
			break
		}
		if !sleep(ctx, waitInterval) {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = fs.ErrorObjectNotFound
	}
	return nil, lastErr
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// About returns the quota information.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	opts := rest.Opts{Method: "GET", Path: "/quote"}
	var quote api.QuoteResponse
	if _, err := f.callJSON(ctx, &opts, nil, &quote); err != nil {
		return nil, err
	}
	free := quote.All.Bytes - quote.Used.Bytes
	if free < 0 {
		free = 0
	}
	trashed := quote.Categories["trash"].Bytes
	return &fs.Usage{
		Total:   fs.NewUsageValue(quote.All.Bytes),
		Used:    fs.NewUsageValue(quote.Used.Bytes),
		Trashed: fs.NewUsageValue(trashed),
		Free:    fs.NewUsageValue(free),
	}, nil
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info {
	return o.fs
}

// String returns a description of the Object.
func (o *Object) String() string {
	return o.remote
}

// Remote returns the remote path.
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns the modification time.
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

// Size returns the size of the object.
func (o *Object) Size() int64 {
	return o.size
}

// Hash returns no hash because SberDisk does not expose one.
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return "", nil
}

// Storable returns true.
func (o *Object) Storable() bool {
	return true
}

// MimeType returns the MIME type of the object.
func (o *Object) MimeType(ctx context.Context) string {
	return o.mimeType
}

// ID returns the SberDisk ID of the object.
func (o *Object) ID() string {
	return strconv.FormatInt(o.id, 10)
}

// fullPath returns the absolute path of the object.
func (o *Object) fullPath() string {
	return o.fs.absPath(o.remote)
}

// SetModTime is not supported.
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorCantSetModTime
}

// Open opens the object for reading.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.FixRangeOption(options, o.size)

	link := o.downloadURL
	if link == "" {
		link = fmt.Sprintf("%s/download/file/%d?purpose=download&filename=%s", downloadURL, o.id, url.QueryEscape(o.name))
	}
	u, err := url.Parse(link)
	if err != nil {
		return nil, err
	}
	opts := rest.Opts{
		Method:     "GET",
		RootURL:    u.Scheme + "://" + u.Host,
		Path:       u.Path,
		Parameters: u.Query(),
		Options:    options,
	}
	resp, err := o.fs.call(ctx, &opts)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Update updates the object.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	obj, err := o.fs.Put(ctx, in, src, options...)
	if err != nil {
		return err
	}
	newObj, ok := obj.(*Object)
	if !ok {
		return errors.New("sberdisk: unexpected object type after upload")
	}
	*o = *newObj
	return nil
}

// Remove removes the object.
func (o *Object) Remove(ctx context.Context) error {
	err := o.fs.deleteIDs(ctx, []string{strconv.FormatInt(o.id, 10)})
	if isNotFound(err) {
		return fs.ErrorObjectNotFound
	}
	return err
}

// parseTime parses the JavaScript date strings returned by SberDisk.
func parseTime(item *api.File) time.Time {
	for _, value := range []string{item.LastChangeAt, item.UploadAt, item.CreatedAt} {
		if value == "" {
			continue
		}
		for _, layout := range []string{
			"Mon Jan 02 2006 15:04:05 GMT-0700",
			"Mon Jan 2 2006 15:04:05 GMT-0700",
			time.RFC3339,
			"2006-01-02 15:04:05",
		} {
			if t, err := time.Parse(layout, value); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

var retryErrorCodes = []int{
	429, // Too Many Requests
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
}

func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

func errorHandler(resp *http.Response) error {
	errResponse := new(api.ErrorResponse)
	err := rest.DecodeJSON(resp, errResponse)
	if err != nil {
		fs.Debugf(nil, "Couldn't decode error response: %v", err)
		return fmt.Errorf("SberDisk HTTP error %v: %w", resp.Status, err)
	}
	errResponse.StatusCode = resp.StatusCode
	if errResponse.Message == "" {
		errResponse.Message = resp.Status
	}
	return errResponse
}

func isNotFound(err error) bool {
	var apiErr *api.ErrorResponse
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound)
}

// Check the interfaces are satisfied
var (
	_ fs.Fs        = (*Fs)(nil)
	_ fs.Purger    = (*Fs)(nil)
	_ fs.Abouter   = (*Fs)(nil)
	_ fs.Copier    = (*Fs)(nil)
	_ fs.Mover     = (*Fs)(nil)
	_ fs.DirMover  = (*Fs)(nil)
	_ fs.Object    = (*Object)(nil)
	_ fs.MimeTyper = (*Object)(nil)
	_ fs.IDer      = (*Object)(nil)
)
