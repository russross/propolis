//
// Propolis: Amazon S3 <--> local file system synchronizer
// Copyright © 2011 Russ Ross <russ@russross.com>
//
// This file is part of Propolis
//
// Propolis is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 2 of the License, or
// (at your option) any later version.
// 
// Propolis is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
// 
// You should have received a copy of the GNU General Public License
// along with Propolis.  If not, see <http://www.gnu.org/licenses/>.
//

// Synchronization logic

package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
)

func (p *Propolis) UpdateFile(elt *File) (err os.Error) {
	// see what is in the local file system
	var er os.Error
	elt.LocalInfo, er = os.Lstat(elt.LocalPath)

	if er != nil {
		// make sure info is nil as a signal that the file doesn't exist or is not accessible
		elt.LocalInfo = nil
	} else {
		elt.LocalInfo.Name = elt.ServerPath
	}

	// see what is on the server
	if err = p.GetFileInfo(elt); err != nil {
		return
	}
	switch {
	case elt.ServerInfo == nil && p.Refresh:
		if err = p.StatRequest(elt); err != nil {
			return
		}
		if elt.ServerInfo != nil && elt.ServerHashHex != "" {
			// the cache appears to be out of date, so update it
			fmt.Printf("Adding missing cache entry [%s]\n", elt.ServerPath)
			if err = p.SetFileInfo(elt, false); err != nil {
				return
			}
		}

	case elt.ServerInfo != nil && !p.TrustCache:
		cacheinfo := elt.ServerInfo
		cachehash := elt.ServerHashHex
		elt.ServerInfo = nil
		elt.ServerHashHex = ""
		if err = p.StatRequest(elt); err != nil {
			return
		}
		if elt.ServerInfo == nil || elt.ServerHashHex == "" {
			// cache said we had something, server disagrees
			fmt.Printf("Removing bogus cache entry [%s]\n", elt.ServerPath)
			if err = p.DeleteFileInfo(elt); err != nil {
				return
			}
		} else {
			// see if the server and the cache disagree
			if cachehash != elt.ServerHashHex ||
				cacheinfo.Uid != elt.ServerInfo.Uid ||
				cacheinfo.Gid != elt.ServerInfo.Gid ||
				cacheinfo.Mode != elt.ServerInfo.Mode ||
				cacheinfo.Mtime_ns != elt.ServerInfo.Mtime_ns ||
				cacheinfo.Size != elt.ServerInfo.Size {

				fmt.Printf("Updating bogus cache entry [%s]\n", elt.ServerPath)
				if err = p.SetFileInfo(elt, false); err != nil {
					return
				}
			}
		}
	}

	// now compare
	switch {
	case elt.LocalInfo == nil && elt.ServerInfo == nil:
		// nothing to do
		fmt.Printf("No such file locally or on server [%s]\n", elt.ServerPath)
		return

	case elt.LocalInfo == nil && elt.ServerInfo != nil:
		// delete the file
		fmt.Printf("Deleting file [%s]\n", elt.ServerPath)

		// delete the file before the metadata: if something goes wrong, the
		// delete request will be repeated on reload, but that's better than
		// leaving a dead file on the server and forgetting about it
		if err = p.DeleteRequest(elt); err != nil {
			return
		}
		// delete the cache entry
		if err = p.DeleteFileInfo(elt); err != nil {
			return
		}
		return

	case elt.LocalInfo != nil && elt.ServerInfo == nil ||
		elt.LocalInfo.Mode != elt.ServerInfo.Mode ||
		elt.LocalInfo.Uid != elt.ServerInfo.Uid ||
		elt.LocalInfo.Gid != elt.ServerInfo.Gid ||
		elt.LocalInfo.Size != elt.ServerInfo.Size ||
		elt.LocalInfo.Mtime_ns != elt.ServerInfo.Mtime_ns:
		// server needs an update

		// clear cache entry first: if something fails, the update
		// will be repeated on restart
		if elt.ServerInfo != nil {
			if err = p.DeleteFileInfo(elt); err != nil {
				return
			}
		}

		// is this a kind of file we don't track?
		if !elt.LocalInfo.IsRegular() &&
			!elt.LocalInfo.IsSymlink() &&
			(!p.Directories || !elt.LocalInfo.IsDirectory()) {
			if elt.ServerInfo != nil {
				// the current file must have replaced an old regular file
				fmt.Printf("Deleting old file masked by untracked file [%s]\n", elt.ServerPath)
				if err = p.DeleteRequest(elt); err != nil {
					return
				}
				if err = p.DeleteFileInfo(elt); err != nil {
					return
				}
			} else {
				fmt.Printf("Ignoring untracked file [%s]\n", elt.ServerPath)
			}

			return
		}

		// is it an empty file?
		if elt.LocalInfo.Size == 0 || elt.LocalInfo.IsDirectory() {
			fmt.Printf("Uploading zero-length file [%s]\n", elt.ServerPath)
			if err = p.UploadRequest(elt); err != nil {
				return
			}
			if err = p.SetFileInfo(elt, true); err != nil {
				return
			}
			return
		}

		// get the md5sum of the local file
		if err = p.GetMd5(elt); err != nil {
			return
		}

		// elt.Contents is live now, so make sure it gets closed

		var src string
		if elt.LocalHashHex == elt.ServerHashHex {
			// this is just a metadata update with no content change
			src = elt.ServerPath
		} else {
			// look for another file with the same contents
			// so we can do a server-to-server copy
			if src, err = p.GetPathFromMd5(elt); err != nil {
				elt.Contents.Close()
				return
			}
		}

		if src == "" {
			// upload the file
			fmt.Printf("Uploading [%s]\n", elt.ServerPath)
			if err = p.UploadRequest(elt); err != nil {
				// elt.Contents is closed by upload
				return
			}
			if err = p.SetFileInfo(elt, true); err != nil {
				return
			}
			return
		}

		// copy an existing file
		fmt.Printf("Copying file [%s] to [%s]\n", src, elt.ServerPath)
		if err = p.CopyRequest(elt, "/"+p.Bucket+src); err != nil {
			// copy failed, so try a regular upload
			fmt.Printf("Copy failed, uploading [%s]\n", elt.ServerPath)
			if err = p.UploadRequest(elt); err != nil {
				// elt.Contents is closed by upload
				return
			}
		} else {
			elt.Contents.Close()
		}
		if err = p.SetFileInfo(elt, true); err != nil {
			return
		}

		return

	default:
		if !p.Paranoid && elt.LocalInfo.Size > 0 {
			// check md5sum for a match
			if err = p.GetMd5(elt); err != nil {
				return
			}

			// upload if different
			if elt.LocalHashHex != elt.ServerHashHex {
				fmt.Printf("MD5 mismatch, uploading [%s]\n", elt.ServerPath)
				if err = p.UploadRequest(elt); err != nil {
					return
				}
				if err = p.SetFileInfo(elt, true); err != nil {
					return
				}
			} else {
				fmt.Printf("No change [%s]\n", elt.ServerPath)
				elt.Contents.Close()
			}
		}
	}

	return
}

// open a file and compute an md5 hash for its contents
// this fills in the hash values and sets the Contents field
// to an open file handle ready to read the file
// if file has Size == 0, this function does nothing
func (p *Propolis) GetMd5(elt *File) (err os.Error) {
	// don't bother for empty files
	if elt.LocalInfo.Size == 0 || elt.LocalInfo.IsDirectory() {
		return
	}

	hash := md5.New()

	// is it a symlink?
	if elt.LocalInfo.IsSymlink() {
		// read the link
		var target string
		if target, err = os.Readlink(elt.LocalPath); err != nil {
			return
		}

		// compute the hash
		hash.Write([]byte(target))

		// wrap it up as an io.ReadCloser
		elt.Contents = ioutil.NopCloser(bytes.NewBufferString(target))
	} else {
		// regular file
		var fp *os.File
		if fp, err = os.Open(elt.LocalPath); err != nil {
			return
		}

		// compute md5 hash
		if _, err = io.Copy(hash, fp); err != nil {
			fp.Close()
			return
		}
		// rewind the file
		if _, err = fp.Seek(0, 0); err != nil {
			fp.Close()
			return
		}
		elt.Contents = fp
	}

	// get the hash in hex
	sum := hash.Sum()
	elt.LocalHashHex = hex.EncodeToString(sum)

	// and in base64
	var buf bytes.Buffer
	encoder := base64.NewEncoder(base64.StdEncoding, &buf)
	encoder.Write(sum)
	encoder.Close()
	elt.LocalHashBase64 = buf.String()

	return
}
