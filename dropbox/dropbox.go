// Package dropbox provides an interface to Dropbox object storage
package dropbox

// FIXME buffer chunks for retries in upload
// FIXME dropbox for business would be quite easy to add

/*
The Case folding of PathDisplay problem

From the docs:

path_display String. The cased path to be used for display purposes
only. In rare instances the casing will not correctly match the user's
filesystem, but this behavior will match the path provided in the Core
API v1, and at least the last path component will have the correct
casing. Changes to only the casing of paths won't be returned by
list_folder/continue. This field will be null if the file or folder is
not mounted. This field is optional.

We solve this by not implementing the ListR interface.  The dropbox remote will recurse directory by directory and all will be well.
*/

import (
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ncw/dropbox-sdk-go-unofficial/dropbox"
	"github.com/ncw/dropbox-sdk-go-unofficial/dropbox/files"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/oauthutil"
	"github.com/ncw/rclone/pacer"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
)

// Constants
const (
	rcloneClientID              = "5jcck7diasz0rqy"
	rcloneEncryptedClientSecret = "fRS5vVLr2v6FbyXYnIgjwBuUAt0osq_QZTXAEcmZ7g"
	minSleep                    = 10 * time.Millisecond
	maxSleep                    = 2 * time.Second
	decayConstant               = 2 // bigger for slower decay, exponential
)

var (
	// Description of how to auth for this app
	dropboxConfig = &oauth2.Config{
		Scopes: []string{},
		// Endpoint: oauth2.Endpoint{
		// 	AuthURL:  "https://www.dropbox.com/1/oauth2/authorize",
		// 	TokenURL: "https://api.dropboxapi.com/1/oauth2/token",
		// },
		Endpoint:     dropbox.OAuthEndpoint(""),
		ClientID:     rcloneClientID,
		ClientSecret: fs.MustReveal(rcloneEncryptedClientSecret),
		RedirectURL:  oauthutil.RedirectLocalhostURL,
	}
	// A regexp matching path names for files Dropbox ignores
	// See https://www.dropbox.com/en/help/145 - Ignored files
	ignoredFiles = regexp.MustCompile(`(?i)(^|/)(desktop\.ini|thumbs\.db|\.ds_store|icon\r|\.dropbox|\.dropbox.attr)$`)
	// Upload chunk size - setting too small makes uploads slow.
	// Chunks aren't buffered into memory though so can set large.
	uploadChunkSize    = fs.SizeSuffix(128 * 1024 * 1024)
	maxUploadChunkSize = fs.SizeSuffix(150 * 1024 * 1024)
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "dropbox",
		Description: "Dropbox",
		NewFs:       NewFs,
		Config: func(name string) {
			err := oauthutil.ConfigNoOffline("dropbox", name, dropboxConfig)
			if err != nil {
				log.Fatalf("Failed to configure token: %v", err)
			}
		},
		Options: []fs.Option{{
			Name: "app_key",
			Help: "Dropbox App Key - leave blank normally.",
		}, {
			Name: "app_secret",
			Help: "Dropbox App Secret - leave blank normally.",
		}},
	})
	fs.VarP(&uploadChunkSize, "dropbox-chunk-size", "", fmt.Sprintf("Upload chunk size. Max %v.", maxUploadChunkSize))
}

// Fs represents a remote dropbox server
type Fs struct {
	name           string       // name of this remote
	root           string       // the path we are working on
	features       *fs.Features // optional features
	srv            files.Client // the connection to the dropbox server
	slashRoot      string       // root with "/" prefix, lowercase
	slashRootSlash string       // root with "/" prefix and postfix, lowercase
	pacer          *pacer.Pacer // To pace the API calls
}

// Object describes a dropbox object
//
// Dropbox Objects always have full metadata
type Object struct {
	fs      *Fs       // what this object is part of
	remote  string    // The remote path
	bytes   int64     // size of the object
	modTime time.Time // time it was last modified
	hash    string    // content_hash of the object
}

// ------------------------------------------------------------

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
	return fmt.Sprintf("Dropbox root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// shouldRetry returns a boolean as to whether this err deserves to be
// retried.  It returns the err as a convenience
func shouldRetry(err error) (bool, error) {
	if err == nil {
		return false, err
	}
	baseErrString := errors.Cause(err).Error()
	// FIXME there is probably a better way of doing this!
	if strings.Contains(baseErrString, "too_many_write_operations") || strings.Contains(baseErrString, "too_many_requests") {
		return true, err
	}
	return fs.ShouldRetry(err), err
}

// NewFs contstructs an Fs from the path, container:path
func NewFs(name, root string) (fs.Fs, error) {
	if uploadChunkSize > maxUploadChunkSize {
		return nil, errors.Errorf("chunk size too big, must be < %v", maxUploadChunkSize)
	}

	// Convert the old token if it exists.  The old token was just
	// just a string, the new one is a JSON blob
	oldToken := strings.TrimSpace(fs.ConfigFileGet(name, fs.ConfigToken))
	if oldToken != "" && oldToken[0] != '{' {
		fs.Infof(name, "Converting token to new format")
		newToken := fmt.Sprintf(`{"access_token":"%s","token_type":"bearer","expiry":"0001-01-01T00:00:00Z"}`, oldToken)
		err := fs.ConfigSetValueAndSave(name, fs.ConfigToken, newToken)
		if err != nil {
			return nil, errors.Wrap(err, "NewFS convert token")
		}
	}

	oAuthClient, _, err := oauthutil.NewClient(name, dropboxConfig)
	if err != nil {
		log.Fatalf("Failed to configure dropbox: %v", err)
	}

	config := dropbox.Config{
		Verbose: false,       // enables verbose logging in the SDK
		Client:  oAuthClient, // maybe???
	}
	srv := files.New(config)

	f := &Fs{
		name:  name,
		srv:   srv,
		pacer: pacer.New().SetMinSleep(minSleep).SetMaxSleep(maxSleep).SetDecayConstant(decayConstant),
	}
	f.features = (&fs.Features{CaseInsensitive: true, ReadMimeType: true}).Fill(f)
	f.setRoot(root)

	// See if the root is actually an object
	_, err = f.getFileMetadata(f.slashRoot)
	if err == nil {
		newRoot := path.Dir(f.root)
		if newRoot == "." {
			newRoot = ""
		}
		f.setRoot(newRoot)
		// return an error with an fs which points to the parent
		return f, fs.ErrorIsFile
	}
	return f, nil
}

// Sets root in f
func (f *Fs) setRoot(root string) {
	f.root = strings.Trim(root, "/")
	lowerCaseRoot := strings.ToLower(f.root)

	f.slashRoot = "/" + lowerCaseRoot
	f.slashRootSlash = f.slashRoot
	if lowerCaseRoot != "" {
		f.slashRootSlash += "/"
	}
}

// getMetadata gets the metadata for a file or directory
func (f *Fs) getMetadata(objPath string) (entry files.IsMetadata, notFound bool, err error) {
	err = f.pacer.Call(func() (bool, error) {
		entry, err = f.srv.GetMetadata(&files.GetMetadataArg{Path: objPath})
		return shouldRetry(err)
	})
	if err != nil {
		switch e := err.(type) {
		case files.GetMetadataAPIError:
			switch e.EndpointError.Path.Tag {
			case files.LookupErrorNotFound:
				notFound = true
				err = nil
			}
		}
	}
	return
}

// getFileMetadata gets the metadata for a file
func (f *Fs) getFileMetadata(filePath string) (fileInfo *files.FileMetadata, err error) {
	entry, notFound, err := f.getMetadata(filePath)
	if err != nil {
		return nil, err
	}
	if notFound {
		return nil, fs.ErrorObjectNotFound
	}
	fileInfo, ok := entry.(*files.FileMetadata)
	if !ok {
		return nil, fs.ErrorNotAFile
	}
	return fileInfo, nil
}

// getDirMetadata gets the metadata for a directory
func (f *Fs) getDirMetadata(dirPath string) (dirInfo *files.FolderMetadata, err error) {
	entry, notFound, err := f.getMetadata(dirPath)
	if err != nil {
		return nil, err
	}
	if notFound {
		return nil, fs.ErrorDirNotFound
	}
	dirInfo, ok := entry.(*files.FolderMetadata)
	if !ok {
		return nil, fs.ErrorIsFile
	}
	return dirInfo, nil
}

// Return an Object from a path
//
// If it can't be found it returns the error fs.ErrorObjectNotFound.
func (f *Fs) newObjectWithInfo(remote string, info *files.FileMetadata) (fs.Object, error) {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	var err error
	if info != nil {
		err = o.setMetadataFromEntry(info)
	} else {
		err = o.readEntryAndSetMetadata()
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(remote string) (fs.Object, error) {
	return f.newObjectWithInfo(remote, nil)
}

// Strips the root off path and returns it
func strip(path, root string) (string, error) {
	if len(root) > 0 {
		if root[0] != '/' {
			root = "/" + root
		}
		if root[len(root)-1] != '/' {
			root += "/"
		}
	} else if len(root) == 0 {
		root = "/"
	}
	if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(root)) {
		return "", errors.Errorf("path %q is not under root %q", path, root)
	}
	return path[len(root):], nil
}

// Strips the root off path and returns it
func (f *Fs) stripRoot(path string) (string, error) {
	return strip(path, f.slashRootSlash)
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(dir string) (entries fs.DirEntries, err error) {
	root := f.slashRoot
	if dir != "" {
		root += "/" + dir
	}

	started := false
	var res *files.ListFolderResult
	for {
		if !started {
			arg := files.ListFolderArg{
				Path:      root,
				Recursive: false,
			}
			if root == "/" {
				arg.Path = "" // Specify root folder as empty string
			}
			err = f.pacer.Call(func() (bool, error) {
				res, err = f.srv.ListFolder(&arg)
				return shouldRetry(err)
			})
			if err != nil {
				switch e := err.(type) {
				case files.ListFolderAPIError:
					switch e.EndpointError.Path.Tag {
					case files.LookupErrorNotFound:
						err = fs.ErrorDirNotFound
					}
				}
				return nil, err
			}
			started = false
		} else {
			arg := files.ListFolderContinueArg{
				Cursor: res.Cursor,
			}
			err = f.pacer.Call(func() (bool, error) {
				res, err = f.srv.ListFolderContinue(&arg)
				return shouldRetry(err)
			})
			if err != nil {
				return nil, errors.Wrap(err, "list continue")
			}
		}
		for _, entry := range res.Entries {
			var fileInfo *files.FileMetadata
			var folderInfo *files.FolderMetadata
			var metadata *files.Metadata
			switch info := entry.(type) {
			case *files.FolderMetadata:
				folderInfo = info
				metadata = &info.Metadata
			case *files.FileMetadata:
				fileInfo = info
				metadata = &info.Metadata
			default:
				fs.Errorf(f, "Unknown type %T", entry)
				continue
			}

			entryPath := metadata.PathDisplay // FIXME  PathLower

			if folderInfo != nil {
				name, err := f.stripRoot(entryPath + "/")
				if err != nil {
					return nil, err
				}
				name = strings.Trim(name, "/")
				if name != "" && name != dir {
					d := &fs.Dir{
						Name: name,
						When: time.Now(),
						//When:  folderInfo.ClientMtime,
						//Bytes: folderInfo.Bytes,
						//Count: -1,
					}
					entries = append(entries, d)
				}
			} else if fileInfo != nil {
				path, err := f.stripRoot(entryPath)
				if err != nil {
					return nil, err
				}
				o, err := f.newObjectWithInfo(path, fileInfo)
				if err != nil {
					return nil, err
				}
				entries = append(entries, o)
			}
		}
		if !res.HasMore {
			break
		}
	}
	return entries, nil
}

// A read closer which doesn't close the input
type readCloser struct {
	in io.Reader
}

// Read bytes from the object - see io.Reader
func (rc *readCloser) Read(p []byte) (n int, err error) {
	return rc.in.Read(p)
}

// Dummy close function
func (rc *readCloser) Close() error {
	return nil
}

// Put the object
//
// Copy the reader in to the new object which is returned
//
// The new object may have been created if an error is returned
func (f *Fs) Put(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// Temporary Object under construction
	o := &Object{
		fs:     f,
		remote: src.Remote(),
	}
	return o, o.Update(in, src, options...)
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(dir string) error {
	root := path.Join(f.slashRoot, dir)

	// can't create or run metadata on root
	if root == "/" {
		return nil
	}

	// check directory doesn't exist
	_, err := f.getDirMetadata(root)
	if err == nil {
		return nil // directory exists already
	} else if err != fs.ErrorDirNotFound {
		return err // some other error
	}

	// create it
	arg2 := files.CreateFolderArg{
		Path: root,
	}
	err = f.pacer.Call(func() (bool, error) {
		_, err = f.srv.CreateFolder(&arg2)
		return shouldRetry(err)
	})
	return err
}

// Rmdir deletes the container
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(dir string) error {
	root := path.Join(f.slashRoot, dir)

	// can't remove root
	if root == "/" {
		return errors.New("can't remove root directory")
	}

	// check directory exists
	_, err := f.getDirMetadata(root)
	if err != nil {
		return errors.Wrap(err, "Rmdir")
	}

	// check directory empty
	arg := files.ListFolderArg{
		Path:      root,
		Recursive: false,
	}
	if root == "/" {
		arg.Path = "" // Specify root folder as empty string
	}
	var res *files.ListFolderResult
	err = f.pacer.Call(func() (bool, error) {
		res, err = f.srv.ListFolder(&arg)
		return shouldRetry(err)
	})
	if err != nil {
		return errors.Wrap(err, "Rmdir")
	}
	if len(res.Entries) != 0 {
		return errors.New("directory not empty")
	}

	// remove it
	err = f.pacer.Call(func() (bool, error) {
		_, err = f.srv.Delete(&files.DeleteArg{Path: root})
		return shouldRetry(err)
	})
	return err
}

// Precision returns the precision
func (f *Fs) Precision() time.Duration {
	return time.Second
}

// Copy src to this remote using server side copy operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}

	// Temporary Object under construction
	dstObj := &Object{
		fs:     f,
		remote: remote,
	}

	// Copy
	arg := files.RelocationArg{}
	arg.FromPath = srcObj.remotePath()
	arg.ToPath = dstObj.remotePath()
	var err error
	var entry files.IsMetadata
	err = f.pacer.Call(func() (bool, error) {
		entry, err = f.srv.Copy(&arg)
		return shouldRetry(err)
	})
	if err != nil {
		return nil, errors.Wrap(err, "copy failed")
	}

	// Set the metadata
	fileInfo, ok := entry.(*files.FileMetadata)
	if !ok {
		return nil, fs.ErrorNotAFile
	}
	err = dstObj.setMetadataFromEntry(fileInfo)
	if err != nil {
		return nil, errors.Wrap(err, "copy failed")
	}

	return dstObj, nil
}

// Purge deletes all the files and the container
//
// Optional interface: Only implement this if you have a way of
// deleting all the files quicker than just running Remove() on the
// result of List()
func (f *Fs) Purge() (err error) {
	// Let dropbox delete the filesystem tree
	err = f.pacer.Call(func() (bool, error) {
		_, err = f.srv.Delete(&files.DeleteArg{Path: f.slashRoot})
		return shouldRetry(err)
	})
	return err
}

// Move src to this remote using server side move operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantMove
func (f *Fs) Move(src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}

	// Temporary Object under construction
	dstObj := &Object{
		fs:     f,
		remote: remote,
	}

	// Do the move
	arg := files.RelocationArg{}
	arg.FromPath = srcObj.remotePath()
	arg.ToPath = dstObj.remotePath()
	var err error
	var entry files.IsMetadata
	err = f.pacer.Call(func() (bool, error) {
		entry, err = f.srv.Move(&arg)
		return shouldRetry(err)
	})
	if err != nil {
		return nil, errors.Wrap(err, "move failed")
	}

	// Set the metadata
	fileInfo, ok := entry.(*files.FileMetadata)
	if !ok {
		return nil, fs.ErrorNotAFile
	}
	err = dstObj.setMetadataFromEntry(fileInfo)
	if err != nil {
		return nil, errors.Wrap(err, "move failed")
	}
	return dstObj, nil
}

// DirMove moves src, srcRemote to this remote at dstRemote
// using server side move operations.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantDirMove
//
// If destination exists then return fs.ErrorDirExists
func (f *Fs) DirMove(src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}
	srcPath := path.Join(srcFs.slashRoot, srcRemote)
	dstPath := path.Join(f.slashRoot, dstRemote)

	// Check if destination exists
	_, err := f.getDirMetadata(f.slashRoot)
	if err == nil {
		return fs.ErrorDirExists
	} else if err != fs.ErrorDirNotFound {
		return err
	}

	// Make sure the parent directory exists
	// ...apparently not necessary

	// Do the move
	arg := files.RelocationArg{}
	arg.FromPath = srcPath
	arg.ToPath = dstPath
	err = f.pacer.Call(func() (bool, error) {
		_, err = f.srv.Move(&arg)
		return shouldRetry(err)
	})
	if err != nil {
		return errors.Wrap(err, "MoveDir failed")
	}

	return nil
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() fs.HashSet {
	return fs.HashSet(fs.HashDropbox)
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Hash returns the dropbox special hash
func (o *Object) Hash(t fs.HashType) (string, error) {
	if t != fs.HashDropbox {
		return "", fs.ErrHashUnsupported
	}
	err := o.readMetaData()
	if err != nil {
		return "", errors.Wrap(err, "failed to read hash from metadata")
	}
	return o.hash, nil
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.bytes
}

// setMetadataFromEntry sets the fs data from a files.FileMetadata
//
// This isn't a complete set of metadata and has an inacurate date
func (o *Object) setMetadataFromEntry(info *files.FileMetadata) error {
	o.bytes = int64(info.Size)
	o.modTime = info.ClientModified
	o.hash = info.ContentHash
	return nil
}

// Reads the entry for a file from dropbox
func (o *Object) readEntry() (*files.FileMetadata, error) {
	return o.fs.getFileMetadata(o.remotePath())
}

// Read entry if not set and set metadata from it
func (o *Object) readEntryAndSetMetadata() error {
	// Last resort set time from client
	if !o.modTime.IsZero() {
		return nil
	}
	entry, err := o.readEntry()
	if err != nil {
		return err
	}
	return o.setMetadataFromEntry(entry)
}

// Returns the remote path for the object
func (o *Object) remotePath() string {
	return o.fs.slashRootSlash + o.remote
}

// Returns the key for the metadata database for a given path
func metadataKey(path string) string {
	// NB File system is case insensitive
	path = strings.ToLower(path)
	hash := md5.New()
	_, _ = hash.Write([]byte(path))
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// Returns the key for the metadata database
func (o *Object) metadataKey() string {
	return metadataKey(o.remotePath())
}

// readMetaData gets the info if it hasn't already been fetched
func (o *Object) readMetaData() (err error) {
	if !o.modTime.IsZero() {
		return nil
	}
	// Last resort
	return o.readEntryAndSetMetadata()
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime() time.Time {
	err := o.readMetaData()
	if err != nil {
		fs.Debugf(o, "Failed to read metadata: %v", err)
		return time.Now()
	}
	return o.modTime
}

// SetModTime sets the modification time of the local fs object
//
// Commits the datastore
func (o *Object) SetModTime(modTime time.Time) error {
	// Dropbox doesn't have a way of doing this so returning this
	// error will cause the file to be deleted first then
	// re-uploaded to set the time.
	return fs.ErrorCantSetModTimeWithoutDelete
}

// Storable returns whether this object is storable
func (o *Object) Storable() bool {
	return true
}

// Open an object for read
func (o *Object) Open(options ...fs.OpenOption) (in io.ReadCloser, err error) {
	headers := fs.OpenOptionHeaders(options)
	arg := files.DownloadArg{Path: o.remotePath(), ExtraHeaders: headers}
	err = o.fs.pacer.Call(func() (bool, error) {
		_, in, err = o.fs.srv.Download(&arg)
		return shouldRetry(err)
	})

	switch e := err.(type) {
	case files.DownloadAPIError:
		// Don't attempt to retry copyright violation errors
		if e.EndpointError.Path.Tag == files.LookupErrorRestrictedContent {
			return nil, fs.NoRetryError(err)
		}
	}

	return
}

// uploadChunked uploads the object in parts
//
// Call only if size is >= uploadChunkSize
//
// FIXME buffer chunks to improve upload retries
func (o *Object) uploadChunked(in io.Reader, commitInfo *files.CommitInfo, size int64) (entry *files.FileMetadata, err error) {
	chunkSize := int64(uploadChunkSize)
	chunks := int(size/chunkSize) + 1

	// write the first whole chunk
	fs.Debugf(o, "Uploading chunk 1/%d", chunks)
	var res *files.UploadSessionStartResult
	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		res, err = o.fs.srv.UploadSessionStart(&files.UploadSessionStartArg{}, &io.LimitedReader{R: in, N: chunkSize})
		return shouldRetry(err)
	})
	if err != nil {
		return nil, err
	}

	cursor := files.UploadSessionCursor{
		SessionId: res.SessionId,
		Offset:    uint64(chunkSize),
	}
	appendArg := files.UploadSessionAppendArg{
		Cursor: &cursor,
		Close:  false,
	}

	// write more whole chunks (if any)
	for i := 2; i < chunks; i++ {
		fs.Debugf(o, "Uploading chunk %d/%d", i, chunks)
		err = o.fs.pacer.CallNoRetry(func() (bool, error) {
			err = o.fs.srv.UploadSessionAppendV2(&appendArg, &io.LimitedReader{R: in, N: chunkSize})
			return shouldRetry(err)
		})
		if err != nil {
			return nil, err
		}
		cursor.Offset += uint64(chunkSize)
	}

	// write the remains
	args := &files.UploadSessionFinishArg{
		Cursor: &cursor,
		Commit: commitInfo,
	}
	fs.Debugf(o, "Uploading chunk %d/%d", chunks, chunks)
	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		entry, err = o.fs.srv.UploadSessionFinish(args, in)
		return shouldRetry(err)
	})
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// Update the already existing object
//
// Copy the reader into the object updating modTime and size
//
// The new object may have been created if an error is returned
func (o *Object) Update(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	remote := o.remotePath()
	if ignoredFiles.MatchString(remote) {
		fs.Logf(o, "File name disallowed - not uploading")
		return nil
	}
	commitInfo := files.NewCommitInfo(o.remotePath())
	commitInfo.Mode.Tag = "overwrite"
	// The Dropbox API only accepts timestamps in UTC with second precision.
	commitInfo.ClientModified = src.ModTime().UTC().Round(time.Second)

	size := src.Size()
	var err error
	var entry *files.FileMetadata
	if size > int64(uploadChunkSize) {
		entry, err = o.uploadChunked(in, commitInfo, size)
	} else {
		err = o.fs.pacer.CallNoRetry(func() (bool, error) {
			entry, err = o.fs.srv.Upload(commitInfo, in)
			return shouldRetry(err)
		})
	}
	if err != nil {
		return errors.Wrap(err, "upload failed")
	}
	return o.setMetadataFromEntry(entry)
}

// Remove an object
func (o *Object) Remove() (err error) {
	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		_, err = o.fs.srv.Delete(&files.DeleteArg{Path: o.remotePath()})
		return shouldRetry(err)
	})
	return err
}

// Check the interfaces are satisfied
var (
	_ fs.Fs       = (*Fs)(nil)
	_ fs.Copier   = (*Fs)(nil)
	_ fs.Purger   = (*Fs)(nil)
	_ fs.Mover    = (*Fs)(nil)
	_ fs.DirMover = (*Fs)(nil)
	_ fs.Object   = (*Object)(nil)
)
